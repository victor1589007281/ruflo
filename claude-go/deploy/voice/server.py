# gf-voice: 通用 ASR 服务 (SenseVoiceSmall)
#
# 定位: claude-go 生态的语音能力件, 与产品无关。不认识任何用户/会话/业务,
# 只干一件事: 16k 单声道 WAV 进, 结构化文本出。音频识别完即弃, 不落盘不落库。
#
# 调用契约:
#   POST /v1/asr
#     - Header: Authorization: Bearer $VOICE_TOKEN   (VOICE_TOKEN 未设置时裸奔, 仅调试用)
#     - Body: 原始 WAV 字节 (Content-Type: audio/wav)
#             必须是 16kHz / 单声道 / PCM16 —— 重采样是客户端的职责
#             (浏览器侧 OfflineAudioContext 重采样, 见 gameforge/webapp 录音组件)
#   → 200 {text, lang, emotion, events, duration_ms, model}
#
# SenseVoiceSmall 的输出文本自带元标签, 本服务解析后返回结构化字段:
#   "<|zh|><|HAPPY|><|withitn|>建一座彩虹桥" → text=建一座彩虹桥 lang=zh emotion=happy
# 情感/事件字段客户端可以暂时忽略, 但接口先白送 (kid app 情感彩蛋的伏笔)。
#
# 并发模型: 单进程单模型 + 推理锁串行。短句推理亚秒级, 排队代价远小于
# 多实例的模型内存翻倍 (936M 参数常驻)。以后要流式/VAD 再谈多副本。
import base64
import io
import os
import re
import secrets
import tempfile
import threading
import time
import wave

from fastapi import FastAPI, Header, HTTPException, Request
from fastapi.responses import JSONResponse

VOICE_MODEL = os.environ.get("VOICE_MODEL", "iic/SenseVoiceSmall")
VOICE_TOKEN = os.environ.get("VOICE_TOKEN", "")
MAX_AUDIO_SECONDS = 30
MAX_BODY_BYTES = 5 * 1024 * 1024

app = FastAPI()
model = None
infer_lock = threading.Lock()

# SenseVoice 元标签全集 (FunASR sensevoice parse_param):
#   语言 / 情感 / 音频事件 / withitn(数字归一开关)。未知标签一律忽略, 不报错。
LANG_TAGS = {"zh", "yue", "en", "ja", "ko", "auto"}
EMOTION_TAGS = {
    "HAPPY", "SAD", "ANGRY", "NEUTRAL",
    "FEARFUL", "DISGUSTED", "SURPRISED",
}
EVENT_TAGS = {
    "LAUGHTER", "CRY", "SIGH", "COUGH", "SNEEZE", "BREATHING",
    "SNORT", "PAT", "YAWN", "SNORE", "HAMMERING", "VIBRATING",
    "FOOTSTEPS", "DOOR_OPEN_CLOSE", "PHONE_RINGING", "MUSIC",
    "TV", "ANIMAL", "KITCHEN", "BABY", "CAR", "WATER", "WIND",
    "SPEECH", "NOISE", "SILENCE",
}
IGNORE_TAGS = {"withitn", "woitn", "emotional", "emo_unknown"}
META_RE = re.compile(r"<\|([^|]+)\|>")


@app.on_event("startup")
def startup():
    global model
    print(f"[gf-voice] loading model {VOICE_MODEL} ...", flush=True)
    t0 = time.time()
    from funasr import AutoModel
    # 模型在构建期已预下载进镜像 (~/.cache/modelscope), 此处直接命中本地缓存。
    # 兜底模型 (paraformer-zh) 与 SenseVoiceSmall 走同一 generate 入口,
    # 仅 SenseVoice 需要 language/use_itn 参数。
    model = AutoModel(model=VOICE_MODEL, device="cpu")
    print(f"[gf-voice] model loaded in {time.time() - t0:.1f}s", flush=True)


def require_token(authorization: str | None):
    if not VOICE_TOKEN:
        return  # 未配置 token = 本地裸奔模式, 仅开发期
    if not authorization or not secrets.compare_digest(
        authorization.removeprefix("Bearer ").strip(), VOICE_TOKEN
    ):
        raise HTTPException(status_code=401, detail="bad voice token")


def parse_meta(raw: str) -> dict:
    lang, emotion, events = None, None, []
    for tag in META_RE.findall(raw):
        upper = tag.upper()
        if tag.lower() in IGNORE_TAGS:
            continue
        if tag in LANG_TAGS:
            lang = tag
        elif upper in EMOTION_TAGS:
            emotion = upper.lower()
        elif upper in EVENT_TAGS:
            events.append(upper.lower())
    text = META_RE.sub("", raw).strip()
    return {"text": text, "lang": lang, "emotion": emotion, "events": events}


def decode_wav(data: bytes) -> float:
    """校验 WAV 契约 (16k/单声道/PCM16), 返回时长秒数。不合格直接 400。"""
    try:
        with wave.open(io.BytesIO(data)) as w:
            ch, sw, sr = w.getnchannels(), w.getsampwidth(), w.getframerate()
            frames = w.getnframes()
    except wave.Error as e:
        raise HTTPException(400, f"not a valid wav: {e}")
    if sr != 16000:
        raise HTTPException(400, f"require 16kHz wav, got {sr}")
    if ch != 1:
        raise HTTPException(400, f"require mono wav, got {ch} channels")
    if sw != 2:
        raise HTTPException(400, f"require PCM16, got sample width {sw}")
    duration = frames / sr
    if duration > MAX_AUDIO_SECONDS:
        raise HTTPException(400, f"audio too long: {duration:.1f}s > {MAX_AUDIO_SECONDS}s")
    return duration


@app.post("/v1/asr")
async def asr(request: Request, authorization: str | None = Header(None)):
    require_token(authorization)
    if model is None:
        raise HTTPException(503, "model not loaded yet")

    ctype = request.headers.get("content-type", "")
    body = await request.body()
    if len(body) > MAX_BODY_BYTES:
        raise HTTPException(413, f"audio too large: {len(body)} bytes")
    if ctype.startswith("application/json"):
        payload = await request.json()
        try:
            body = base64.b64decode(payload["audio"], validate=True)
        except Exception:
            raise HTTPException(400, "json body requires base64 'audio' field")
    duration = decode_wav(body)

    # 写临时文件走 generate(input=path) —— 这是 funasr 全版本最稳的入口,
    # ndarray/fs 参数在不同版本间行为有差异, 不赌。
    t0 = time.time()
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as f:
        f.write(body)
        path = f.name
    try:
        with infer_lock:
            kwargs = {"input": path}
            if "SenseVoice" in VOICE_MODEL:
                kwargs.update(language="auto", use_itn=True)
            res = model.generate(**kwargs)
    finally:
        os.unlink(path)
    latency = time.time() - t0

    raw_text = (res[0].get("text", "") if res else "").strip()
    out = parse_meta(raw_text)
    out.update({
        "duration_ms": round(duration * 1000),
        "latency_ms": round(latency * 1000),
        "model": VOICE_MODEL,
    })
    # 成功也报数: 每次识别落一行日志, 童声调优时靠它统计延迟与空识别率
    print(f"[gf-voice] asr dur={out['duration_ms']}ms lat={out['latency_ms']}ms "
          f"lang={out['lang']} emo={out['emotion']} text={out['text'][:30]!r}", flush=True)
    return JSONResponse(out)


@app.get("/healthz")
def healthz():
    # 进程活着就 200 —— 供 liveness/startup 探针 (模型加载窗口内也必须是 200,
    # 否则 startupProbe 在模型就绪前就把它打死, 永远起不来)
    return {"ok": True, "model": VOICE_MODEL, "model_loaded": model is not None}


@app.get("/readyz")
def readyz():
    if model is None:
        return JSONResponse({"ok": False}, status_code=503)
    return {"ok": True}
