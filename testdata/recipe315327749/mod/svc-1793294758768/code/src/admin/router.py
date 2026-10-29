from fastapi import APIRouter

from src.admin.models import Version
from src.admin.version import get_version

router = APIRouter()


@router.get("/version", response_model=Version)
async def version():
    return get_version()
