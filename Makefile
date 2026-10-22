.PHONY: test seed reseed run mock-provider

test:
	$(MAKE) -C backend test

seed:
	$(MAKE) -C backend seed

reseed:
	$(MAKE) -C backend reseed

run:
	$(MAKE) -C backend run

mock-provider:
	$(MAKE) -C backend mock-provider