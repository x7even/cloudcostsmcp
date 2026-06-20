FROM python:3.12-slim

RUN pip install uv

WORKDIR /app

# Install dependencies only first (cached layer). The project itself is installed
# after src/ is copied, so this layer is reused across source-only changes.
COPY pyproject.toml uv.lock* ./
RUN uv sync --frozen --no-dev --no-install-project

# Copy source + readme, then install the project. hatch-vcs derives the version
# from git, but .git is not in the build context — pass it explicitly via build arg.
COPY src/ src/
COPY README.md ./
ARG OCC_VERSION=0.0.0
ENV SETUPTOOLS_SCM_PRETEND_VERSION=${OCC_VERSION}
RUN uv sync --frozen --no-dev

ENV OCC_HTTP_HOST=0.0.0.0
ENV OCC_HTTP_PORT=8080

EXPOSE 8080

CMD ["uv", "run", "opencloudcosts", "--transport", "http", "--host", "0.0.0.0"]
