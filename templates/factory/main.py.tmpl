from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from codefly.codefly import load_service_configuration

service = load_service_configuration()

app = FastAPI()

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
    return {"version": service.version}
