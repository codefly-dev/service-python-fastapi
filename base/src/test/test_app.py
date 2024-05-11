import sys, os
sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), '..')))

from httpx import AsyncClient, ASGITransport
import pytest
from main import app
from codefly_sdk.codefly import init

@pytest.mark.asyncio
async def test_read_root():
    init("..")
    async with AsyncClient(transport=ASGITransport(app=app), base_url="http://localhost:8080") as ac:
        response = await ac.get("/version")
    assert response.status_code == 200
    assert response.json() == {"version": "0.0.0"}
