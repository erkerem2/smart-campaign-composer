.PHONY: test test-short test-e2e seed reseed run mock-provider frontend-install frontend-dev frontend-build test-frontend

test:
	$(MAKE) -C backend test

test-short:
	$(MAKE) -C backend test-short

test-e2e:
	$(MAKE) -C backend test-e2e

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

test-frontend:
	cd frontend && npm test
