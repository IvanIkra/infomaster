FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.13.7 /uv /bin/

# The environment lives outside /app, so a bind mount over the app directory cannot hide it.
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never UV_PROJECT_ENVIRONMENT=/opt/venv \
    PATH=/opt/venv/bin:$PATH
WORKDIR /app

# Dependencies first, so code changes don't reinstall them. Exact versions come from uv.lock.
COPY pyproject.toml uv.lock .python-version ./
RUN uv sync --locked --no-dev --no-install-project --no-cache

COPY infomaster infomaster

# The service runs as a plain user (D79): a flaw in a parser does not run as root over the data directory. APP_UID
# is the owner of data/ on the host (.env), so its files stay the host user's; the code stays root's, read-only.
ARG APP_UID=1000
RUN useradd --uid "$APP_UID" --user-group --no-create-home --home-dir /app/data --shell /usr/sbin/nologin app
USER app

# 8080: public (the web app and the connector, behind Funnel); 8081: internal (API for Tablo and HA)
EXPOSE 8080 8081
CMD ["python", "-m", "infomaster", "--data", "data"]
