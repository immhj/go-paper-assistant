FROM python:3.13.7-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir --requirement requirements.txt
COPY app.py ieee.py spec_terms.py venue_ranks.py ./
# Pinned, vendored rank data (ADR 0020); hashes are verified against manifest.json at start-up.
COPY data/manifest.json data/ccf_rank_list.tsv data/impact_factor.sqlite3 data/THIRD_PARTY_NOTICES.txt ./data/

RUN useradd --create-home --uid 10001 app
USER app
EXPOSE 8090
CMD ["python", "/app/app.py"]
