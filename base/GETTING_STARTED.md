## Welcome to your `fastapi` project!

This agent follows the [fastapi best practices](https://github.com/zhanymkanov/fastapi-best-practices) layout.

Dependency management uses [uv](https://docs.astral.sh/uv/). Common commands:

- `uv sync` — install/refresh the venv from `pyproject.toml` + `uv.lock`
- `uv add <package>` — add a runtime dependency
- `uv add --dev <package>` — add a dev dependency
- `uv run pytest` — run tests
- `uv run uvicorn src.main:app --reload` — run the dev server

`codefly run service <name>` does all of the above for you via the agent.
