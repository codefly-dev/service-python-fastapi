import json
from fastapi.openapi.utils import get_openapi

from main import app
from codefly.codefly import service

from fastapi.openapi.utils import get_openapi

if __name__ == "__main__":
    openapi_schema = get_openapi(
        title=service.name,
        version=service.version,
        routes=app.routes,
    )
    app.openapi_schema = openapi_schema
    openapi = app.openapi()
    with open("swagger/api.swagger.json", "w") as f:
        f.write(json.dumps(openapi))
