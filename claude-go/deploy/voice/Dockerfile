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

COPY server.py /app/server.py
WORKDIR /app
# hostNetwork 下 k8s 要求 hostPort == containerPort, 容器直接监听宿主机端口 38795,
# 不经过端口映射 —— 这就是"Pod 重建外部调用不变"的代价与承诺
EXPOSE 38795
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "38795"]
