from dataclasses import dataclass, field
from typing import Optional, Callable

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
    startup: Optional[Callable] = None
    shutdown: Optional[Callable] = None
    migrations: list[str] = field(default_factory=list)
