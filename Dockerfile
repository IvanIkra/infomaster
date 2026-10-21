FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /app

# Dependencies first, so code changes don't reinstall them. tzdata: zoneinfo needs it, slim has none.
COPY pyproject.toml .
RUN python -c "import tomllib; print('\n'.join(tomllib.load(open('pyproject.toml', 'rb'))['project']['dependencies']))" > /tmp/req.txt \
    && pip install --no-cache-dir -r /tmp/req.txt tzdata \
    && rm /tmp/req.txt

COPY uni_master uni_master

# 8080: public (Mini App, behind Funnel); 8081: internal (API for Tablo and HA)
EXPOSE 8080 8081
CMD ["python", "-m", "uni_master", "--data", "data"]
