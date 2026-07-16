# 轻量 Python 运行时(multi-arch 官方镜像,含 linux/arm64)
FROM python:3.13-slim

WORKDIR /app

# 拷贝应用代码
COPY server.py .

# 声明应用监听端口
EXPOSE 8080

# 启动应用 —— Lambda 在应用完成初始化后对运行态拍快照
CMD ["python", "server.py"]
