from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
import codefly.codefly as codefly

codefly.init()

app = FastAPI()

if codefly.is_local():
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


@app.get("/version")
async def version():
    return {"version": codefly.get_service().version}
