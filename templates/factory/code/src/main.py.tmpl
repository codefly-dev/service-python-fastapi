from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
import codefly_sdk.codefly as codefly

codefly.init()

app = FastAPI()

# CORS will be done properly in next release
origins = [
    "*",
]
app.add_middleware(
    CORSMiddleware,
    allow_origins=origins,
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# Routes
from src.admin.router import router as admin
app.include_router(admin)

# Plugins
from src.plugins.registry import plugins

for plugin in plugins:
    app.include_router(plugin.router, prefix=plugin.prefix, tags=plugin.tags)
    if plugin.startup:
        app.add_event_handler("startup", plugin.startup)
    if plugin.shutdown:
        app.add_event_handler("shutdown", plugin.shutdown)

if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app)
