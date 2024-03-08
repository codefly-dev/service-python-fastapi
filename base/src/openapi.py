import json
from main import app
import codefly.codefly as codefly

from fastapi.openapi.utils import get_openapi

if __name__ == "__main__":
    service = codefly.service()
    openapi_schema = get_openapi(
        title=service.name,
        version=service.version,
        routes=app.routes,
    )
    app.openapi_schema = openapi_schema
    openapi = app.openapi()
    with open("../openapi/api.swagger.json", "w") as f:
        f.write(json.dumps(openapi))
