FROM python:3.12-slim

RUN pip install uv

WORKDIR /app
COPY pyproject.toml uv.lock* ./
RUN uv sync --frozen --no-dev 2>/dev/null || uv sync --no-dev

COPY src/ src/

ENV OCC_HTTP_HOST=0.0.0.0
ENV OCC_HTTP_PORT=8080

EXPOSE 8080

CMD ["uv", "run", "opencloudcosts", "--transport", "http", "--host", "0.0.0.0"]
