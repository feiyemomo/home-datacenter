FROM python:3.11-alpine

WORKDIR /app

COPY h3c_client.py main.py ./

ENV PYTHONUNBUFFERED=1
ENV TZ=Asia/Shanghai

EXPOSE 8087

CMD ["python", "main.py"]
