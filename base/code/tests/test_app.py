import pytest
from httpx import AsyncClient, ASGITransport

from src.main import app

import codefly_sdk.codefly as codefly


@pytest.mark.asyncio
async def test_read_root():
    codefly.init("..")
    endpoint = codefly.endpoint(api="rest")
    async with AsyncClient(transport=ASGITransport(app=app), base_url=endpoint.address) as ac:
        response = await ac.get("/version")
    assert response.status_code == 200
    assert response.json() == {"version": "0.0.0"}
