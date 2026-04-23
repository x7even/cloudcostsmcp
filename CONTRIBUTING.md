# Contributing to OpenCloudCosts

Thanks for helping improve OpenCloudCosts. This guide covers the fast path from idea to merged PR.

## Dev setup

```bash
git clone https://github.com/x7even/cloudcostmcp
cd cloudcostmcp
uv sync --extra dev
uv run pytest          # all tests green
uv run ruff check src/ # lint clean
```

Requires [uv](https://docs.astral.sh/uv/).

## Running tests

```bash
uv run pytest                          # full suite
uv run pytest tests/test_providers/   # provider unit tests only
uv run pytest -k "aws"                 # filter by name
```

Tests mock the upstream pricing APIs — no cloud credentials needed.

## Code style

```bash
uv run ruff check src/ --fix   # auto-fix safe issues
uv run ruff format src/        # format
```

Line length 100. Select: E, F, I, UP (excluding E501 and UP042 — see pyproject.toml).

## Submitting a PR

1. Fork the repo and create a branch from `main`.
2. Write tests for new behaviour. Provider unit tests live in `tests/test_providers/`.
3. Run `uv run pytest` and `uv run ruff check src/` — both must pass.
4. Open a PR. CI runs automatically on push.

## Adding a new provider capability

1. Add the capability tuple to `_CAPABILITIES` in the provider class.
2. Add a price-lookup branch in `get_price()`.
3. Add a `describe_catalog()` entry with an `example_invocation`.
4. Add a contract test case in `tests/test_providers/test_provider_contract.py`.
5. Add a unit test for the new lookup.

## Fixing a pricing bug

If a tool returns a wrong price:
1. Find the upstream SKU that should match (AWS Pricing console, GCP Cloud Billing catalog,
   Azure Retail Prices API).
2. Add a failing test with a minimal fixture replicating the SKU structure.
3. Fix the filter/parser in the provider, confirm the test passes.
4. Note the SKU ID or description in the commit message for traceability.

## Questions

Open a [GitHub Discussion](https://github.com/x7even/cloudcostmcp/discussions) for usage
questions. File a [GitHub Issue](https://github.com/x7even/cloudcostmcp/issues) for bugs
or feature requests.
