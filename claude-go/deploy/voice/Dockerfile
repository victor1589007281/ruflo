# gf-voice 通用 ASR 服务镜像
# 构建期完成全部重活: 依赖安装 + 模型预下载 (ModelScope 国内源),
# Pod 启动即就绪, 运行期零外网依赖。
FROM python:3.11-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    VOICE_MODEL=iic/SenseVoiceSmall

# torch/torchaudio 必须装在前: funasr 依赖 torch, 先装 CPU-only 版 (PyPI 默认是 CUDA 版,
# 会拖进 2GB+ nvidia 依赖), funasr 检测到 torch 已满足就不再装。
# torchaudio 是 fbank 特征提取的后端之一 (缺它运行期才炸 ImportError, 构建期不报错),
# 版本必须与 torch 匹配, 同一 CPU index 安装可保证。
RUN pip install torch torchaudio --index-url https://download.pytorch.org/whl/cpu \
 && pip install -i https://mirrors.aliyun.com/pypi/simple/ \
      fastapi "uvicorn[standard]" funasr modelscope

# 预下载模型进镜像层 (~/.cache/modelscope), 各自独立成层 —— 单个下载失败
# 不至于让另一个的重下:
#   SenseVoiceSmall        —— 默认, 中英粤日韩 + 情感/事件标签
#   paraformer-zh (large)  —— 童声 A/B 兜底, 改 VOICE_MODEL 环境变量即切换
#   (注意模型 id 带 -pytorch 后缀, 不带会 404)
RUN python -c "from modelscope import snapshot_download as d; d('iic/SenseVoiceSmall')"
RUN python -c "from modelscope import snapshot_download as d; d('iic/speech_paraformer-large-vad-punc_asr_nat-zh-cn-16k-common-vocab8404-pytorch')"

# ── 流式 ASR: silero-vad (词边界检测, wheel 自带模型文件, 零外网)
#    构建期必须验证可加载 —— 上次 torchaudio 的教训: 缺推理后端构建期不报错运行期才炸
RUN pip install -i https://mirrors.aliyun.com/pypi/simple/ silero-vad \
 && python -c "from silero_vad import load_silero_vad; m=load_silero_vad(); import torch; print('silero ok', float(m(torch.zeros(512), 16000)))"

# ── TTS: CosyVoice2 (FunASR 同生态, 本地离线)
#    CosyVoice 是"仓库即包"形态: 克隆整仓 + third_party/Matcha-TTS 进 sys.path,
#    不单独发 pypi 包。slim 镜像无 git, 先装; pyworld 无预编译 wheel 需源码编译, 带 g++。
#    依赖清单手挑运行时子集
#    (去 torch/torchaudio —— 已有 CPU 版, 防止 pip 为满足依赖拖回 2GB+ CUDA 版),
#    构建期 import 冒烟验证。
RUN apt-get update && apt-get install -y --no-install-recommends git g++ && rm -rf /var/lib/apt/lists/*
RUN git config --global http.lowSpeedLimit 0 && git config --global http.lowSpeedTime 999 \
 && git clone --depth 1 https://github.com/FunAudioLLM/CosyVoice.git /opt/CosyVoice
RUN pip install -i https://mirrors.aliyun.com/pypi/simple/ \
      onnxruntime pypinyin zhon inflect WeTextProcessing jieba soundfile \
      omegaconf hydra-core "pydantic>=2" hyperpyyaml safetensors rich GPUtil tqdm addict \
      openai-whisper conformer einops lightning matplotlib gdown librosa wget \
      pyarrow pyworld \
      "transformers==4.46.3" "huggingface_hub==0.26.5" "diffusers==0.31.0" \
 && PYTHONPATH=/opt/CosyVoice:/opt/CosyVoice/third_party/Matcha-TTS \
    python -c "import cosyvoice.cli.cosyvoice; print('cosyvoice import ok')"
# third_party/Matcha-TTS 是 git submodule, --depth 1 不会带, 必须显式拉 ——
# 缺了它构建期不报错, 运行期加载模型 yaml 时才炸 No module named 'matcha' (pydoc.safeimport 路径)。
# matcha 只被当作库 import (flow_matching 组件), torch/torchaudio 已齐, 走 PYTHONPATH 即可。
# github 低速链路会触发 git 默认低速中止, 上面已关 lowSpeed 检查; submodule 也走浅克隆。
RUN cd /opt/CosyVoice && git config --global http.lowSpeedLimit 0 \
 && git submodule update --init --depth 1 --recursive
# 全量冒烟: cli + flow_matching 都要能 import (上次只测 cli, 漏了 matcha)
RUN PYTHONPATH=/opt/CosyVoice:/opt/CosyVoice/third_party/Matcha-TTS \
    python -c "import cosyvoice.cli.cosyvoice, cosyvoice.flow.flow_matching; print('cosyvoice full import ok')"
# CosyVoice2-0.5B: 中英粤 SFT 多发音人 + 支持流式合成; 预下载进镜像层
RUN python -c "from modelscope import snapshot_download as d; d('iic/CosyVoice2-0.5B')"
# 发音人表注入: CosyVoice2 官方仓库不带 spk2info.pt (只支持 zero-shot),
# 用 CosyVoice 1.0 (iic/CosyVoice-300M-SFT) 的 7 发音人表 (campplus 192维), 合成质量经 ASR 回验
RUN python -c "import os, urllib.request; from modelscope import snapshot_download as d; md = d('iic/CosyVoice2-0.5B'); urllib.request.urlretrieve('https://modelscope.cn/models/iic/CosyVoice-300M-SFT/resolve/master/spk2info.pt', os.path.join(md, 'spk2info.pt')); print('spk2info injected into', md)"
COPY server.py /app/server.py
WORKDIR /app
# 端到端合成冒烟: 走生产代码路径 (load_tts + synth_wav), 校验出声/时长/采样率。
# 历史上 TTS 的三类缺口 (pyarrow/pyworld 缺包、transformers v5 静默劣化、dtype 边界)
# 全是 import 冒烟测不出来的 —— 必须真合成一段并断言能量。
RUN python -c "import sys, torch, wave, io; sys.path.insert(0, '/opt/CosyVoice'); sys.path.insert(0, '/opt/CosyVoice/third_party/Matcha-TTS'); import server; server.load_tts(); assert server.COSY is not None, server.TTS_LOAD_ERR; wav = server.synth_wav('你好，我们一起搭彩虹桥吧。', '中文女'); w = wave.open(io.BytesIO(wav)); n, sr = w.getnframes(), w.getframerate(); assert sr == server.COSY_SR == 24000 and n > sr // 2, (sr, n); print('synthesis smoke ok sr=%s frames=%s' % (sr, n))"

# hostNetwork 下 k8s 要求 hostPort == containerPort, 容器直接监听宿主机端口 38795,
# 不经过端口映射 —— 这就是"Pod 重建外部调用不变"的代价与承诺
EXPOSE 38795
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "38795"]
