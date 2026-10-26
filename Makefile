.PHONY: test test-short test-e2e test-frontend test-all test-docker \
        seed reseed run mock-provider frontend-install frontend-dev frontend-build \
        up up-d down logs ps docker-reseed docker-clean

# ---------------------------------------------------------------- tests -----
# Go suite: mock LLM + in-process mock provider, no API key, no network.
test:
	$(MAKE) -C backend test

test-short:
	$(MAKE) -C backend test-short

test-e2e:
	$(MAKE) -C backend test-e2e

test-frontend:
	cd frontend && npm test

test-all: test test-frontend

# Both suites inside the build images: needs only Docker (no Go, no Node).
test-docker:
	docker build --target test ./backend
	docker build --target test ./frontend

# ------------------------------------------------------ local (no Docker) ---
seed:
	$(MAKE) -C backend seed

reseed:
	$(MAKE) -C backend reseed

run:
	$(MAKE) -C backend run

mock-provider:
	$(MAKE) -C backend mock-provider

frontend-install:
	cd frontend && npm install

frontend-dev:
	cd frontend && npm run dev

frontend-build:
	cd frontend && npm run build

# ------------------------------------------------------- docker compose -----
up:
	docker compose up --build

up-d:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f --tail=100

ps:
	docker compose ps

# Wipe users, campaigns and dispatch logs in the volume and seed 3,000 fresh
# users (dates are relative to now, so "last 30 days" cohorts stay current).
docker-reseed:
	docker compose stop backend
	docker compose run --rm seed -reset
	docker compose start backend

# Remove containers and the database volume.
docker-clean:
	docker compose down -v --remove-orphans
