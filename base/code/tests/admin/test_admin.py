import pytest
from httpx import AsyncClient, ASGITransport

from src.main import app

import codefly_sdk.codefly as codefly

from src.admin.version import get_version
from src.admin.models import Version



@pytest.mark.asyncio
async def test_version():
    codefly.init("..")
    endpoint = codefly.endpoint(api="rest")
    address = endpoint.address if endpoint else "http://localhost:8080"
    async with AsyncClient(transport=ASGITransport(app=app), base_url=address) as ac:
        response = await ac.get("/version")
    assert response.status_code == 200

    assert Version.model_validate(response.json()) == get_version()
