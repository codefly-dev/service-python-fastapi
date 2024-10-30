# This script generates the OpenAPI schema for the FastAPI application and saves it to a file.
# This is used by the agent - please do not modify
import json
import os

from src.main import app

import codefly_sdk.codefly as codefly
from fastapi.openapi.utils import get_openapi

if __name__ == "__main__":
    current_file_path = os.path.abspath(__file__)
    openapi_dir = os.path.abspath(os.path.join(current_file_path, "../../../openapi"))
    openapi_file_path = os.path.join(openapi_dir, "api.swagger.json")

    openapi_schema = get_openapi(
        title=codefly.get_service(),
        version=codefly.get_version(),
        routes=app.routes,
    )
    app.openapi_schema = openapi_schema
    openapi = app.openapi()
    with open(openapi_file_path, "w") as f:
        f.write(json.dumps(openapi))
