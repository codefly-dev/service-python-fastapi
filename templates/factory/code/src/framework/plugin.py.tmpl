from collections.abc import Callable
from dataclasses import dataclass, field

from fastapi import APIRouter


@dataclass
class Plugin:
    """Plugin interface for FastAPI service extensions.

    Each plugin contributes a router, optional startup/shutdown hooks,
    and optional migration paths.
    """

    name: str
    router: APIRouter
    prefix: str = ""
    tags: list[str] = field(default_factory=list)
    startup: Callable | None = None
    shutdown: Callable | None = None
    migrations: list[str] = field(default_factory=list)
