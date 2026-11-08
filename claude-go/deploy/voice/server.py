"""
gf-voice 通用语音服务 (claude-go 生态能力件)
=============================================
与产品无关: 不认识任何用户/会话, 音频/文本不落盘不落库 (临时 wav 转录后即删)。

能力:
  POST /v1/asr          一次性识别: 16kHz/mono/PCM16 WAV 字节 → {text,lang,emotion,events}
  WS   /v1/asr/stream   流式识别: 客户端推 PCM 帧, 服务端 silero-vad 自动断句,
                        每句完成推一条 result —— 前端"免按键"对话的基础
  POST /v1/tts          合成: {text, spk} → WAV 字节 (CosyVoice2 SFT, 本地离线)

统一鉴权: Authorization: Bearer $VOICE_TOKEN (常量时间比较; VOICE_TOKEN 为空 = 开发裸奔)
所有模型推理串行化 (单副本小规格 Pod, 防多请求并发把 CPU 打满导致全员超时)

部署: kind-hakit agents ns, hostNetwork Pod 持 38795 (kind 节点 netns) + 宿主机
转发器对外稳定 127.0.0.1:38795 / 100.111.70.76:38795 —— 任何重建外部配置不变。
"""
import base64
import io
import json
import logging
import os
import sys
import tempfile
import threading
import time
import wave
from collections import deque

import numpy as np
import torch
import uvicorn
from fastapi import FastAPI, Request, Response, WebSocket, WebSocketDisconnect
from fastapi.responses import JSONResponse
from starlette.websockets import WebSocketDisconnected  # receive() after disconnect — 客户端主动断开的正常路径

VOICE_TOKEN = os.environ.get("VOICE_TOKEN", "")
VOICE_MODEL = os.environ.get("VOICE_MODEL", "iic/SenseVoiceSmall")
TTS_ENABLED = os.environ.get("TTS_ENABLED", "1") != "0"
TTS_MODEL_ID = os.environ.get("TTS_MODEL_ID", "iic/CosyVoice2-0.5B")
TTS_HOME = os.environ.get("COSYVOICE_HOME", "/opt/CosyVoice")
OMP_NUM_THREADS = os.environ.get("OMP_NUM_THREADS", "4")
torch.set_num_threads(int(OMP_NUM_THREADS))

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger("gf-voice")

app = FastAPI(title="gf-voice", docs_url=None, redoc_url=None)

# 串行化模型推理: 单副本 Pod, 并发推理只会互相拖慢 (CPU 打满 → 全员超时)
INFER_LOCK = threading.Lock()
MODEL = None            # funasr AutoModel (ASR)
COSY = None             # CosyVoice2 实例 (TTS)
COSY_SR = 22050         # 合成采样率 (模型自带属性, 加载后校正)
VAD = None              # silero-vad (流式端点检测)
TTS_LOADING = threading.Event()
TTS_LOAD_ERR = None

ASR_READY = False

# SenseVoiceSmall 输出文本自带 <|zh|><|HAPPY|><|withitn|> 元信息标签, 解析成结构化字段
LANG_TAGS = {"zh", "yue", "en", "ja", "ko", "auto"}
EMOTION_TAGS = {"HAPPY", "SAD", "ANGRY", "NEUTRAL", "FEARFUL", "DISGUSTED", "SURPRISED", "UNKNOWN"}
EVENT_TAGS = {
    "withitn", "woitn", "Speech", "Applause", "BGM", "Laughter", "Cry",
    "Sneeze", "Breath", "Cough", "Noise", "Spill", "Slam", "Ring",
    "Hum", "Tts", "None",
}


def parse_meta(text: str):
    lang, emotion, events = None, None, []
    out = []
    for part in text.split("<|"):
        if "|>" not in part:
            out.append(part)
            continue
        tag, rest = part.split("|>", 1)
        tag = tag.strip()
        if tag in LANG_TAGS:
            lang = tag
        elif tag in EMOTION_TAGS:
            emotion = tag.lower()
        elif tag in EVENT_TAGS:
            events.append(tag.lower())
        # SenseVoice 的元标签全部前缀在正文之前, 正文跟在最后一个标签后 (rest)
        out.append(rest)
    return "".join(out).strip(), lang, emotion, events


def check_auth(req) -> bool:
    """常量时间比较 Bearer token (timing-attack 防护)"""
    import hmac

    auth = req.headers.get("Authorization", "")
    token = auth[7:] if auth.startswith("Bearer ") else ""
    if not VOICE_TOKEN:
        return True  # 开发裸奔模式, 生产必须用 Secret 注入
    return hmac.compare_digest(token.encode(), VOICE_TOKEN.encode())


def load_asr():
    global MODEL, ASR_READY
    from funasr import AutoModel

    log.info("loading asr model %s ...", VOICE_MODEL)
    MODEL = AutoModel(
        model=VOICE_MODEL,
        device="cpu",
        disable_update=True,
        # SenseVoiceSmall 生成时用贪心解码即可, 输出已含标点 (ITN)
    )
    ASR_READY = True
    log.info("asr model ready")


def load_tts():
    """CosyVoice2 懒加载: ASR 就绪后在后台线程加载, 不拖慢就绪探针"""
    global COSY, COSY_SR, TTS_LOAD_ERR
    if not TTS_ENABLED:
        return
    TTS_LOADING.set()
    try:
        log.info("loading cosyvoice tts %s ...", TTS_MODEL_ID)
        # CosyVoice 是"仓库即包"形态, 本体 + third_party/Matcha-TTS 都要进 sys.path
        sys.path.insert(0, TTS_HOME)
        sys.path.insert(0, os.path.join(TTS_HOME, "third_party", "Matcha-TTS"))
        from cosyvoice.cli.cosyvoice import CosyVoice2  # noqa: E402
        from modelscope import snapshot_download

        model_dir = snapshot_download(TTS_MODEL_ID)  # 构建期已预下载, 命中镜像内缓存
        cosy = CosyVoice2(model_dir, load_jit=False, load_trt=False, fp16=False)
        #  dtype 规整 (CPU 实测结论, 勿删):
        #  transformers 4.x 把 Qwen encoder 加载为 fp32, 全 fp32 能跑但 RTF≈4 太慢;
        #  llm 整体转 bf16 (Zen4 avx512_bf16 硬件加速) → RTF≈1.6。
        #  边界在两处: llm_embedding 必须 bf16 进 LLM; flow 仍是 fp32, flow_embedding 保持 fp32。
        cosy.model.llm.bfloat16()
        _orig_frontend_sft = cosy.frontend.frontend_sft

        def _frontend_sft(text: str, spk: str):
            mi = _orig_frontend_sft(text, spk)
            mi["llm_embedding"] = mi["llm_embedding"].to(torch.bfloat16)
            mi["flow_embedding"] = mi["flow_embedding"].to(torch.float32)
            return mi

        cosy.frontend.frontend_sft = _frontend_sft
        COSY = cosy
        COSY_SR = int(getattr(cosy, "sample_rate", 24000) or 24000)
        log.info("cosyvoice ready sample_rate=%s spks=%s", COSY_SR, cosy.list_available_spks())
    except Exception as e:  # TTS 挂了不影响 ASR 主能力
        TTS_LOAD_ERR = repr(e)
        log.exception("cosyvoice load failed: %s", e)
    finally:
        TTS_LOADING.clear()


def load_vad():
    global VAD
    from silero_vad import load_silero_vad

    VAD = load_silero_vad()
    log.info("silero-vad ready")


@app.on_event("startup")
def startup():
    load_asr()
    load_vad()
    # TTS 2-3GB 模型后台慢加载, 首个 /v1/tts 请求可能撞上 warmup
    threading.Thread(target=load_tts, daemon=True).start()


@app.get("/healthz")
def healthz():
    return {"ok": True}


@app.get("/readyz")
def readyz():
    ok = ASR_READY and VAD is not None
    body = {
        "asr": ASR_READY,
        "vad": VAD is not None,
        "tts": COSY is not None,
        "tts_error": TTS_LOAD_ERR,
        "model": VOICE_MODEL,
    }
    return JSONResponse(body, status_code=200 if ok else 503)


def pcm_to_wav(pcm16: np.ndarray, sample_rate: int) -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(pcm16.astype("<i2").tobytes())
    return buf.getvalue()


def wav_to_pcm16(data: bytes):
    """严格契约: 只收 16kHz/mono/PCM16 的完整 WAV; 其余一律 400"""
    try:
        with wave.open(io.BytesIO(data), "rb") as w:
            ch, width, rate, n = w.getnchannels(), w.getsampwidth(), w.getframerate(), w.getnframes()
            if ch != 1 or width != 2 or rate != 16000:
                raise ValueError(f"need 16kHz/mono/PCM16 wav, got {rate}Hz/{ch}ch/{width * 8}bit")
            if n > 16000 * 30:
                raise ValueError("wav too long (>30s)")
            raw = w.readframes(n)
    except wave.Error as e:
        raise ValueError(f"not a valid wav: {e}")
    return np.frombuffer(raw, dtype="<i2").copy()


def transcribe_pcm16(pcm16: np.ndarray) -> dict:
    """共享转录入口: 临时 wav 落盘 → funasr (path 形式跨版本最稳) → 解析元标签"""
    t0 = time.time()
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
        f.write(pcm_to_wav(pcm16, 16000))
        path = f.name
    try:
        kwargs: dict = {"input": path}
        if "SenseVoice" in VOICE_MODEL:
            kwargs.update(language="auto", use_itn=True)
        with INFER_LOCK:
            result = MODEL.generate(**kwargs)
    finally:
        os.unlink(path)
    raw_text = result[0]["text"]
    text, lang, emotion, events = parse_meta(raw_text)
    latency_ms = int((time.time() - t0) * 1000)
    dur_ms = int(len(pcm16) / 16)  # 16000Hz * 2bytes → ms
    log.info(
        "[gf-voice] asr dur=%dms lat=%dms lang=%s emo=%s text=%s",
        dur_ms, latency_ms, lang, emotion, text[:80],
    )
    return {
        "text": text,
        "lang": lang,
        "emotion": emotion,
        "events": events,
        "duration_ms": dur_ms,
        "latency_ms": latency_ms,
        "final": True,
    }


@app.post("/v1/asr")
async def asr(req: Request):
    if not check_auth(req):
        return JSONResponse({"error": "unauthorized"}, status_code=401)
    body = await req.body()
    if not body:
        return JSONResponse({"error": "empty body"}, status_code=400)
    if req.headers.get("content-type", "").startswith("application/json"):
        # 兼容 base64 包裹: {"audio": "<base64>"}
        try:
            body = base64.b64decode((await req.json()).get("audio", ""))
        except Exception:
            return JSONResponse({"error": "invalid json/base64"}, status_code=400)
    if len(body) > 5 * 1024 * 1024:
        return JSONResponse({"error": "too large"}, status_code=413)
    if not ASR_READY:
        return JSONResponse({"error": "model not ready"}, status_code=503)
    try:
        pcm16 = wav_to_pcm16(body)
    except ValueError as e:
        return JSONResponse({"error": str(e)}, status_code=400)
    return await _run_transcribe(pcm16)


async def _run_transcribe(pcm16: np.ndarray):
    import asyncio

    try:
        result = await asyncio.get_event_loop().run_in_executor(None, transcribe_pcm16, pcm16)
    except Exception as e:
        log.exception("asr failed: %s", e)
        return JSONResponse({"error": "asr failed"}, status_code=500)
    return result


# ─── TTS (CosyVoice2 SFT, 本地离线) ──────────────────────────────────────────
# CosyVoice2-0.5B 官方仓库不带 SFT 发音人 (spk2info.pt 缺省, 官方只支持 zero-shot);
# 这里用 CosyVoice 1.0 (iic/CosyVoice-300M-SFT) 的 spk2info (7 个发音人, campplus 192 维,
# 构建期注入模型目录), 内容正确性经 ASR 回验 —— 勿随意扩名单, 键必须存在于 spk2info。
TTS_SPKS = {
    "中文女", "中文男", "粤语女", "英文男", "英文女", "日语男", "韩语女",
}
TTS_MAX_CHARS = 300


def synth_wav(text: str, spk: str) -> bytes:
    import torchaudio  # 已在镜像内 (ASR 同款 CPU 版)

    t0 = time.time()
    with INFER_LOCK:
        outputs = COSY.inference_sft(text, spk, stream=False, speed=1.0, text_frontend=True)
        audio = next(iter(outputs))["tts_speech"].squeeze(0).cpu().numpy()
    pcm16 = (audio * 32767.0).clip(-32768, 32767).astype("<i2")
    wav = pcm_to_wav(pcm16, COSY_SR)
    log.info(
        "[gf-voice] tts chars=%d audio=%dms@%.1fk gen=%dms",
        len(text), int(len(pcm16) / 32), COSY_SR / 1000, int((time.time() - t0) * 1000),
    )
    return wav


@app.post("/v1/tts")
async def tts(req: Request):
    if not check_auth(req):
        return JSONResponse({"error": "unauthorized"}, status_code=401)
    if not TTS_ENABLED:
        return JSONResponse({"error": "tts disabled"}, status_code=404)
    if COSY is None:
        return JSONResponse(
            {"error": f"tts not ready ({TTS_LOAD_ERR or 'warming up'})"}, status_code=503,
        )
    try:
        body = await req.json()
    except Exception:
        return JSONResponse({"error": "need json body"}, status_code=400)
    text = (body.get("text") or "").strip()
    spk = (body.get("spk") or "中文女").strip()
    if not text:
        return JSONResponse({"error": "empty text"}, status_code=400)
    if len(text) > TTS_MAX_CHARS:
        return JSONResponse({"error": f"text too long (>{TTS_MAX_CHARS})"}, status_code=400)
    if spk not in TTS_SPKS:
        return JSONResponse({"error": f"unknown spk, use one of {sorted(TTS_SPKS)}"}, status_code=400)
    import asyncio

    try:
        wav = await asyncio.get_event_loop().run_in_executor(None, synth_wav, text, spk)
    except Exception as e:
        log.exception("tts failed: %s", e)
        return JSONResponse({"error": "tts failed"}, status_code=500)
    return Response(wav, media_type="audio/wav")


# ─── 流式 ASR (WebSocket + silero-vad 自动断句) ───────────────────────────────
# 协议:
#   客户端 → 服务端  文本帧 {"type":"start","sample_rate":16000,"format":"pcm_s16le"}
#                    二进制帧 = 连续 PCM 帧 (默认 16kHz 有符号 16bit; 也收 f32 与非 16k,
#                    服务端重采样 —— 浏览器 ScriptProcessor 输出即 48k f32)
#                    文本帧 {"type":"stop"} 立即断句转写当前缓冲; {"type":"close"} 结束
#   服务端 → 客户端  {"type":"ready"} 就绪
#                    {"type":"vad","speech":true/false} 仅状态翻转时发 (前端可做"听到了"动效)
#                    {"type":"result",...} 一句完整的识别结果 (同 /v1/asr 字段 + final)
#                    {"type":"error","message":...}
VAD_WINDOW = 512        # silero 在 16k 下的窗口 (32ms)
VAD_STRIDE = 256        # 步长 (16ms), 相邻窗重叠一半, 端点更平滑
VAD_SPEECH_TH = 0.5     # ≥0.5 判有声 (silero 官方推荐阈值)
VAD_SILENCE_TH = 0.35   # <0.35 判静音; 中间地带保持原状态 (迟滞)
ENDPOINT_SILENCE = 0.8  # 句尾静音 ≥0.8s 断句
MIN_SPEECH_S = 0.3      # 短于 0.3s 的"句"丢弃 (咳嗽/敲击)
UTT_MAX_S = 20          # 单句硬顶 20s, 到顶强制断句
MAX_STREAM_CONN = 8     # 单副本全局限流: 并发流式连接上限

STREAM_CONNS = 0
STREAM_CONNS_LOCK = threading.Lock()


class VadSession:
    """单连接的词边界检测状态机。buf 只在 in_speech 期间增长, 20s 硬顶 640KB。"""

    def __init__(self):
        self.leftover = np.zeros(0, dtype=np.float32)
        self.in_speech = False
        self.silence_win = 0
        self.buf = bytearray()      # int16 16k, 句起点之后的全部采样
        self.start = None           # 句起点在 buf 中的偏移

    def _endpoint(self, force: bool):
        audio = b""
        if self.start is not None:
            end = len(self.buf) if force else max(self.start, len(self.buf) - int(0.35 * 32000))
            audio = bytes(self.buf[self.start:end])
        self.buf.clear()
        self.start = None
        self.in_speech = False
        self.silence_win = 0
        if len(audio) < int(MIN_SPEECH_S * 32000):
            return None
        return np.frombuffer(audio, dtype="<i2").copy()

    def feed(self, pcm16: np.ndarray):
        """返回事件列表: ('vad', bool) 状态翻转 / ('utt', pcm16) 断句完成"""
        events = []
        f32 = pcm16.astype(np.float32) / 32768.0
        buf = np.concatenate([self.leftover, f32])
        n_win = (len(buf) - VAD_WINDOW) // VAD_STRIDE + 1
        for i in range(n_win):
            w = buf[i * VAD_STRIDE : i * VAD_STRIDE + VAD_WINDOW]
            prob = float(VAD(torch.from_numpy(w), 16000).item())
            if prob >= VAD_SPEECH_TH:
                if not self.in_speech:
                    self.in_speech = True
                    self.start = len(self.buf)
                    events.append(("vad", True))
                self.silence_win = 0
            elif prob < VAD_SILENCE_TH and self.in_speech:
                self.silence_win += 1
            if self.in_speech:
                s, e = i * VAD_STRIDE, i * VAD_STRIDE + VAD_STRIDE
                self.buf.extend(pcm16[s:e].tobytes())
                if self.silence_win >= int(ENDPOINT_SILENCE / 0.016) or len(self.buf) - self.start >= UTT_MAX_S * 32000:
                    utt = self._endpoint(force=True)
                    events.append(("vad", False))
                    if utt is not None:
                        events.append(("utt", utt))
        keep = max(0, n_win * VAD_STRIDE)
        self.leftover = buf[keep:]
        return events

    def flush(self):
        """客户端 stop: 立即断句 (不落尾巴)"""
        if not self.in_speech:
            return None
        self.in_speech = False
        utt = self._endpoint(force=True)
        return utt


def resample_to_16k(f32: np.ndarray, orig_sr: int) -> np.ndarray:
    if orig_sr == 16000:
        return (f32 * 32768.0).clip(-32768, 32767).astype("<i2")
    import torchaudio

    t = torch.from_numpy(f32).unsqueeze(0)
    out = torchaudio.functional.resample(t, orig_sr, 16000).squeeze(0).numpy()
    return (out * 32768.0).clip(-32768, 32767).astype("<i2")


@app.websocket("/v1/asr/stream")
async def asr_stream(ws: WebSocket):
    global STREAM_CONNS
    # 浏览器 WebSocket 不能自定义 header, 所以同时支持 ?token= 查询参数
    token = ws.query_params.get("token", "")
    if VOICE_TOKEN and token:
        import hmac

        authed = hmac.compare_digest(token.encode(), VOICE_TOKEN.encode())
    else:
        auth_hdr = ws.headers.get("Authorization", "")
        authed = check_auth(type("H", (), {"headers": {"Authorization": auth_hdr}})())
    if not authed:
        await ws.close(code=4401)
        return
    with STREAM_CONNS_LOCK:
        if STREAM_CONNS >= MAX_STREAM_CONN:
            await ws.close(code=4429)
            return
        STREAM_CONNS += 1
    try:
        await ws.accept()
        await ws.send_json({"type": "ready"})
        sample_rate, fmt = 16000, "pcm_s16le"
        vad = VadSession()
        import asyncio

        loop = asyncio.get_running_loop()

        async def emit_utt(utt: np.ndarray):
            result = await loop.run_in_executor(None, transcribe_pcm16, utt)
            await ws.send_json({"type": "result", **result})

        while True:
            msg = await ws.receive()
            if msg.get("bytes") is not None:
                raw = msg["bytes"]
                if fmt == "pcm_f32le":
                    f32 = np.frombuffer(raw, dtype="<f4").copy()
                else:
                    f32 = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
                pcm16 = resample_to_16k(f32, sample_rate)
                if pcm16.size == 0:
                    continue
                for kind, payload in vad.feed(pcm16):
                    if kind == "vad":
                        await ws.send_json({"type": "vad", "speech": payload})
                    elif kind == "utt":
                        await emit_utt(payload)
            elif msg.get("text") is not None:
                try:
                    ctl = json.loads(msg["text"])
                except Exception:
                    await ws.send_json({"type": "error", "message": "bad json control frame"})
                    continue
                ctype = ctl.get("type")
                if ctype == "start":
                    sample_rate = int(ctl.get("sample_rate", 16000) or 16000)
                    fmt = ctl.get("format", "pcm_s16le")
                    if fmt not in ("pcm_s16le", "pcm_f32le"):
                        await ws.send_json({"type": "error", "message": "format must be pcm_s16le|pcm_f32le"})
                        fmt = "pcm_s16le"
                elif ctype == "stop":
                    utt = vad.flush()
                    await ws.send_json({"type": "vad", "speech": False})
                    if utt is not None:
                        await emit_utt(utt)
                elif ctype == "close":
                    break
    except (WebSocketDisconnect, WebSocketDisconnected):
        pass
    except Exception as e:
        log.exception("stream error: %s", e)
        try:
            await ws.send_json({"type": "error", "message": "internal error"})
        except Exception:
            pass
    finally:
        with STREAM_CONNS_LOCK:
            STREAM_CONNS -= 1
        try:
            await ws.close()
        except Exception:
            pass


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=38795)
