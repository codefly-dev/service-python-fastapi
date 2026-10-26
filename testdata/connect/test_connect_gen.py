"""The generated Connect face (templates/connect/connect_gen.py) against a real grpc.aio
server: every protocol it serves answers through that server, interceptors included."""

import asyncio
import base64
import collections
import json
import struct

import connect_gen
import grpc
import httpx
import pytest
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

ENVELOPE = struct.Struct(">BI")


def _register():
    file = descriptor_pb2.FileDescriptorProto(
        name="codefly_connect_test/echo.proto", package="echo.v1", syntax="proto3"
    )
    msg = file.message_type.add(name="Msg")
    msg.field.add(name="text", number=1, type=9, label=1, json_name="text")
    msg.field.add(name="count", number=2, type=5, label=1, json_name="count")
    service = file.service.add(name="Echo")
    for name, client, server in (
        ("Say", False, False),
        ("Count", False, True),
        ("Collect", True, False),
    ):
        service.method.add(
            name=name,
            input_type=".echo.v1.Msg",
            output_type=".echo.v1.Msg",
            client_streaming=client,
            server_streaming=server,
        )
    pool = descriptor_pool.Default()
    pool.AddSerializedFile(file.SerializeToString())
    return message_factory.GetMessageClass(pool.FindMessageTypeByName("echo.v1.Msg"))


Msg = _register()


class Proof(grpc.aio.ServerInterceptor):
    """What a hop-stamped service installs: a call without the proof never reaches it."""

    async def intercept_service(self, continuation, details):
        if dict(details.invocation_metadata).get("x-proof") != "stamp":

            async def refuse(request, context):
                await context.abort(grpc.StatusCode.UNAUTHENTICATED, "no proof")

            return grpc.unary_unary_rpc_method_handler(refuse)
        return await continuation(details)


# What a service passes on from its own gRPC client when a downstream answer is past
# that client's bound: grpcio's own words, naming that client's bound.
PASSED_ON = "Stream removed (CLIENT: Received message larger than max (5000005 vs. 4194304))"


async def say(request, context):
    metadata = dict(context.invocation_metadata())
    if request.text == "deny":
        context.set_trailing_metadata((("model-error", '{"code":"Denied"}'),))
        await context.abort(grpc.StatusCode.PERMISSION_DENIED, "denied: not yours")
    if request.text == "slow":
        await asyncio.sleep(2)
    if request.text == "big":
        # An answer of ``count`` bytes from a request of a few.
        return Msg(text="x" * request.count)
    if request.text == "passing-on":
        await context.abort(grpc.StatusCode.RESOURCE_EXHAUSTED, PASSED_ON)
    await context.send_initial_metadata((("x-answer", "header"),))
    context.set_trailing_metadata((("x-trail", "trailer"), ("x-raw-bin", b"\x00\xff")))
    return Msg(text=f"{request.text}|{metadata.get('x-caller', '')}", count=request.count)


async def count(request, context):
    if request.text == "big":
        yield Msg(text="x" * request.count)
        return
    for n in range(request.count):
        yield Msg(text=request.text, count=n)
    if request.text == "fail":
        await context.abort(grpc.StatusCode.UNAVAILABLE, "gone")


async def collect(requests, context):
    texts = [r.text async for r in requests]
    return Msg(text=",".join(texts), count=len(texts))


def _handler():
    codec = {"request_deserializer": Msg.FromString, "response_serializer": Msg.SerializeToString}
    return grpc.method_handlers_generic_handler(
        "echo.v1.Echo",
        {
            "Say": grpc.unary_unary_rpc_method_handler(say, **codec),
            "Count": grpc.unary_stream_rpc_method_handler(count, **codec),
            "Collect": grpc.stream_unary_rpc_method_handler(collect, **codec),
        },
    )


@pytest.fixture
async def face():
    server = grpc.aio.server(interceptors=(Proof(),))
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    listener = await connect_gen.serve(
        0, grpc_port, host="127.0.0.1", max_message_bytes=1024, read_timeout=2
    )
    try:
        yield listener
    finally:
        await listener.stop()
        await server.stop(None)


def _url(face, method):
    return f"http://127.0.0.1:{face.port}/echo.v1.Echo/{method}"


STAMP = {"x-proof": "stamp", "x-caller": "maya"}


def envelope(data, flags=0):
    return ENVELOPE.pack(flags, len(data)) + data


def frames(body):
    out = []
    while body:
        flags, size = ENVELOPE.unpack_from(body)
        out.append((flags, body[ENVELOPE.size : ENVELOPE.size + size]))
        body = body[ENVELOPE.size + size :]
    return out


async def test_connect_unary_binary_relays_metadata_both_ways(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            content=Msg(text="hi", count=2).SerializeToString(),
            headers={"content-type": "application/proto", **STAMP},
        )
    assert r.status_code == 200, r.text
    assert Msg.FromString(r.content) == Msg(text="hi|maya", count=2)
    assert r.headers["x-answer"] == "header"
    assert r.headers["trailer-x-trail"] == "trailer"
    assert base64.b64decode(r.headers["trailer-x-raw-bin"] + "==") == b"\x00\xff"


async def test_connect_unary_json(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            json={"text": "hi", "count": 3},
            headers={"content-type": "application/json", **STAMP},
        )
    assert r.status_code == 200
    assert r.json() == {"text": "hi|maya", "count": 3}


async def test_every_protocol_passes_the_services_interceptors(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"), json={"text": "hi"}, headers={"content-type": "application/json"}
        )
    assert r.status_code == 401
    assert r.json() == {"code": "unauthenticated", "message": "no proof"}


async def test_connect_unary_error_carries_code_and_trailers(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            json={"text": "deny"},
            headers={"content-type": "application/json", **STAMP},
        )
    assert r.status_code == 403
    assert r.json() == {"code": "permission_denied", "message": "denied: not yours"}
    assert json.loads(r.headers["trailer-model-error"]) == {"code": "Denied"}


async def test_connect_timeout_is_the_calls_deadline(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            json={"text": "slow"},
            headers={"content-type": "application/json", "connect-timeout-ms": "50", **STAMP},
        )
    assert r.status_code == 504
    assert r.json()["code"] == "deadline_exceeded"


async def test_connect_server_stream(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Count"),
            content=envelope(Msg(text="x", count=3).SerializeToString()),
            headers={"content-type": "application/connect+proto", **STAMP},
        )
    assert r.status_code == 200
    out = frames(r.content)
    assert [Msg.FromString(m).count for f, m in out[:-1] if f == 0] == [0, 1, 2]
    assert out[-1][0] == 0x02 and json.loads(out[-1][1]) == {}


async def test_connect_server_stream_error_ends_the_stream(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Count"),
            content=envelope(json.dumps({"text": "fail", "count": 1}).encode()),
            headers={"content-type": "application/connect+json", **STAMP},
        )
    out = frames(r.content)
    assert json.loads(out[0][1]) == {"text": "fail"}
    assert json.loads(out[-1][1])["error"] == {"code": "unavailable", "message": "gone"}


async def test_connect_refuses_a_client_stream_over_http1(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Collect"),
            content=envelope(Msg(text="a").SerializeToString()),
            headers={"content-type": "application/connect+proto", **STAMP},
        )
    end = frames(r.content)[-1]
    assert end[0] == 0x02 and json.loads(end[1])["error"]["code"] == "unimplemented"


async def test_grpc_web_unary_and_trailers(face):
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            content=envelope(Msg(text="web").SerializeToString()),
            headers={"content-type": "application/grpc-web+proto", **STAMP},
        )
    out = frames(r.content)
    assert Msg.FromString(out[0][1]).text == "web|maya"
    assert out[-1][0] == 0x80
    trailers = dict(line.split(": ", 1) for line in out[-1][1].decode().strip().split("\r\n"))
    assert trailers["grpc-status"] == "0" and trailers["x-trail"] == "trailer"


async def test_grpc_on_the_same_listener(face):
    async with grpc.aio.insecure_channel(f"127.0.0.1:{face.port}") as channel:
        metadata = tuple(STAMP.items())
        say_call = channel.unary_unary(
            "/echo.v1.Echo/Say",
            request_serializer=Msg.SerializeToString,
            response_deserializer=Msg.FromString,
        )
        assert (await say_call(Msg(text="g"), metadata=metadata)).text == "g|maya"
        collect_call = channel.stream_unary(
            "/echo.v1.Echo/Collect",
            request_serializer=Msg.SerializeToString,
            response_deserializer=Msg.FromString,
        )
        answer = await collect_call(iter([Msg(text="a"), Msg(text="b")]), metadata=metadata)
        assert answer == Msg(text="a,b", count=2)


async def test_http_refusals(face):
    async with httpx.AsyncClient() as client:
        unknown = await client.post(
            _url(face, "Nope"), content=b"", headers={"content-type": "application/proto"}
        )
        get = await client.get(_url(face, "Say"))
        media = await client.post(
            _url(face, "Say"), content=b"", headers={"content-type": "text/plain"}
        )
        big = await client.post(
            _url(face, "Say"), content=b"x" * 4096, headers={"content-type": "application/proto"}
        )
    assert (unknown.status_code, get.status_code, media.status_code) == (404, 405, 415)
    assert big.status_code == 413
    assert big.text == "content length (4096) exceeds the message bound"


async def test_an_oversized_request_never_reaches_the_service(face):
    # A separate response bound leaves requests past 1024 bytes refused.
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            content=(request := Msg(text="x" * 2048).SerializeToString()),
            headers={"content-type": "application/proto", **STAMP},
        )
    assert r.status_code == 413
    assert r.text == f"content length ({len(request)}) exceeds the message bound"


@pytest.mark.parametrize("size", [393 * 1024, 5 * 1024 * 1024])
async def test_an_answer_larger_than_the_request_bound_is_answered(face, size):
    # The face bounds requests at 1024 bytes; embedding-sized answers and answers
    # above grpcio's default 4 MiB receive bound still come back as proto, and JSON within
    # max_json_response_bytes (4 MiB by default: the conversion is what costs).
    big = Msg(text="big", count=size)
    async with httpx.AsyncClient() as client:
        unary = await client.post(
            _url(face, "Say"),
            content=big.SerializeToString(),
            headers={"content-type": "application/proto", **STAMP},
        )
        as_json = await client.post(
            _url(face, "Say"),
            json={"text": "big", "count": size},
            headers={"content-type": "application/json", **STAMP},
        )
        web = await client.post(
            _url(face, "Say"),
            content=envelope(big.SerializeToString()),
            headers={"content-type": "application/grpc-web+proto", **STAMP},
        )
        stream = await client.post(
            _url(face, "Count"),
            content=envelope(big.SerializeToString()),
            headers={"content-type": "application/connect+proto", **STAMP},
        )
    assert unary.status_code == 200, unary.text
    assert Msg.FromString(unary.content).text == "x" * size
    if size <= connect_gen.DEFAULT_MAX_JSON_RESPONSE_BYTES:
        assert as_json.status_code == 200, as_json.text
        assert as_json.json() == {"text": "x" * size}
    else:
        assert as_json.status_code == 400
        assert "max_json_response_bytes (4194304)" in as_json.json()["message"]
    out = frames(web.content)
    assert Msg.FromString(out[0][1]).text == "x" * size
    assert "grpc-status: 0" in out[-1][1].decode()
    out = frames(stream.content)
    assert Msg.FromString(out[0][1]).text == "x" * size
    assert out[-1] == (0x02, b"{}")


@pytest.mark.parametrize(
    "media",
    [
        "application/json",
        "application/proto",
        "application/connect+proto",
        "application/connect+json",
        "application/grpc-web+proto",
    ],
)
async def test_max_response_bytes_bounds_answers_when_set(media):
    server = grpc.aio.server()
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    listener = None
    try:
        listener = await connect_gen.serve(
            0,
            grpc_port,
            host="127.0.0.1",
            max_message_bytes=1024,
            max_response_bytes=2048,
            max_json_response_bytes=2048,
        )
        bound = "max_json_response_bytes" if "json" in media else "max_response_bytes"
        streaming = media.startswith("application/connect+")
        enveloped = streaming or "grpc-web" in media
        async with httpx.AsyncClient() as client:
            for size in (1024, 4096):
                message = Msg(text="big", count=size)
                body = (
                    json.dumps({"text": "big", "count": size}).encode()
                    if "json" in media
                    else message.SerializeToString()
                )
                r = await client.post(
                    _url(listener, "Count" if streaming else "Say"),
                    content=envelope(body) if enveloped else body,
                    headers={"content-type": media},
                )
                if size == 1024:
                    assert r.status_code == 200, r.text
                    data = frames(r.content)[0][1] if enveloped else r.content
                    text = (
                        json.loads(data)["text"] if "json" in media else Msg.FromString(data).text
                    )
                    assert text == "x" * size
                elif "grpc-web" in media:
                    trailers = frames(r.content)[-1][1].decode()
                    assert "grpc-status: 9" in trailers  # FAILED_PRECONDITION
                    assert "max_response_bytes (2048)" in trailers
                else:
                    if streaming:
                        assert r.status_code == 200
                        assert len(frames(r.content)) == 1
                        error = json.loads(frames(r.content)[0][1])["error"]
                    else:
                        # Not 429: the same call is refused the same way when retried.
                        assert r.status_code == 400
                        error = r.json()
                    assert error["code"] == "failed_precondition"
                    assert error["message"] == (
                        f"the answer (4099 bytes) exceeds the Connect face's {bound} (2048)"
                    ), error
    finally:
        if listener is not None:
            await listener.stop()
        await server.stop(None)


@pytest.mark.parametrize("media", ["application/json", "application/connect+json"])
async def test_the_json_bound_is_checked_before_converting(media, monkeypatch):
    # The JSON bound applies to the proto the service returns, before the conversion that
    # would cost many times it: an answer past it is never converted, and the same
    # answer as proto, within max_response_bytes, still comes back.
    server = grpc.aio.server()
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    converted = []
    to_dict = connect_gen.json_format.MessageToDict
    monkeypatch.setattr(
        connect_gen.json_format,
        "MessageToDict",
        lambda message, **kw: converted.append(message.ByteSize()) or to_dict(message, **kw),
    )
    listener = None
    try:
        listener = await connect_gen.serve(
            0,
            grpc_port,
            host="127.0.0.1",
            max_message_bytes=1024,
            max_response_bytes=2**20,
            max_json_response_bytes=2048,
        )
        streaming = media.startswith("application/connect+")
        body = json.dumps({"text": "big", "count": 4096}).encode()
        async with httpx.AsyncClient() as client:
            proto = await client.post(
                _url(listener, "Say"),
                content=Msg(text="big", count=4096).SerializeToString(),
                headers={"content-type": "application/proto"},
            )
            r = await client.post(
                _url(listener, "Count" if streaming else "Say"),
                content=envelope(body) if streaming else body,
                headers={"content-type": media},
            )
        assert proto.status_code == 200 and len(proto.content) == 4099
        error = json.loads(frames(r.content)[-1][1])["error"] if streaming else r.json()
        assert r.status_code == (200 if streaming else 400)
        assert error == {
            "code": "failed_precondition",
            "message": (
                "the answer (4099 bytes) exceeds the Connect face's max_json_response_bytes (2048)"
            ),
        }
        assert 4099 not in converted
    finally:
        if listener is not None:
            await listener.stop()
        await server.stop(None)


async def test_a_refusal_the_service_passes_on_keeps_its_code_and_words(face):
    # A service passing on its own client's receive-limit refusal (grpcio's words, its
    # client's bound) is not the face refusing an answer: code and words stay the server's.
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            json={"text": "passing-on"},
            headers={"content-type": "application/json", **STAMP},
        )
    assert r.status_code == 429
    assert r.json() == {"code": "resource_exhausted", "message": PASSED_ON}


@pytest.mark.parametrize("option", ["max_response_bytes", "max_json_response_bytes"])
@pytest.mark.parametrize("value", [0, -1])
async def test_an_answer_bound_is_a_positive_size(option, value):
    listener = None
    try:
        with pytest.raises(ValueError, match=option):
            listener = await connect_gen.serve(0, 1, host="127.0.0.1", **{option: value})
    finally:
        if listener is not None:
            await listener.stop()


async def test_a_raised_message_bound_raises_the_answer_bound():
    # max_response_bytes defaults to the larger of DEFAULT_MAX_RESPONSE_BYTES and
    # max_message_bytes: a service that raised its bound to 100 MiB answers 70 MiB.
    server = grpc.aio.server(options=[("grpc.max_send_message_length", 100 * 2**20)])
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    listener = await connect_gen.serve(
        0, grpc_port, host="127.0.0.1", max_message_bytes=100 * 2**20
    )
    try:
        size = 70 * 2**20
        async with httpx.AsyncClient(timeout=60) as client:
            r = await client.post(
                _url(listener, "Say"),
                content=Msg(text="big", count=size).SerializeToString(),
                headers={"content-type": "application/proto"},
            )
        assert r.status_code == 200, r.text[:200]
        assert len(Msg.FromString(r.content).text) == size
    finally:
        await listener.stop()
        await server.stop(None)


async def test_the_default_answer_bound_is_16_mib():
    server = grpc.aio.server()
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    listener = await connect_gen.serve(0, grpc_port, host="127.0.0.1")
    try:
        async with httpx.AsyncClient(timeout=60) as client:
            within = await client.post(
                _url(listener, "Say"),
                content=Msg(text="big", count=16 * 2**20 - 8).SerializeToString(),
                headers={"content-type": "application/proto"},
            )
            past = await client.post(
                _url(listener, "Say"),
                content=Msg(text="big", count=16 * 2**20).SerializeToString(),
                headers={"content-type": "application/proto"},
            )
        assert within.status_code == 200
        assert past.status_code == 400
        assert "max_response_bytes (16777216)" in past.json()["message"]
    finally:
        await listener.stop()
        await server.stop(None)


async def test_one_connection_serves_several_calls(face):
    reader, writer = await asyncio.open_connection("127.0.0.1", face.port)
    body = json.dumps({"text": "k"}).encode()
    request = (
        f"POST /echo.v1.Echo/Say HTTP/1.1\r\nhost: x\r\ncontent-type: application/json\r\n"
        f"x-proof: stamp\r\ncontent-length: {len(body)}\r\n\r\n"
    ).encode() + body
    writer.write(request + request)
    await writer.drain()
    seen = b""
    while seen.count(b"HTTP/1.1 200") < 2:
        chunk = await asyncio.wait_for(reader.read(65536), 2)
        assert chunk, seen
        seen += chunk
    writer.close()


async def test_connect_json_ignores_unknown_fields(face):
    # connect-go's JSON codec discards unknown fields: a client built from a newer
    # contract reaches a Python service exactly as it reaches a Go one.
    async with httpx.AsyncClient() as client:
        r = await client.post(
            _url(face, "Say"),
            json={"text": "hi", "count": 1, "addedLater": {"x": 1}},
            headers={"content-type": "application/json", **STAMP},
        )
    assert r.status_code == 200, r.text
    assert r.json() == {"text": "hi|maya", "count": 1}


async def test_a_silent_grpc_connection_is_closed_after_the_idle_bound():
    # An upstream that accepts and never answers: only the face's own bound ends it.
    upstream = await asyncio.start_server(lambda r, w: None, "127.0.0.1", 0)
    listener = await connect_gen.serve(
        0,
        upstream.sockets[0].getsockname()[1],
        host="127.0.0.1",
        read_timeout=0.5,
        idle_timeout=0.5,
    )
    try:
        reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
        writer.write(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
        await writer.drain()
        assert await asyncio.wait_for(reader.read(), 3) == b""
        writer.close()
    finally:
        await listener.stop()
        upstream.close()


async def test_a_grpc_connection_carrying_traffic_outlives_the_idle_bound():
    # Bytes in either direction keep the relay open: a server stream whose client sends
    # nothing is not idle.
    async def drip(reader, writer):
        writer.write(SETTINGS)
        for n in range(6):
            writer.write(h2_frame(0x6, 0, 0, bytes([n]) * 8))
            await writer.drain()
            await asyncio.sleep(0.2)
        writer.close()

    upstream = await asyncio.start_server(drip, "127.0.0.1", 0)
    listener = await connect_gen.serve(
        0, upstream.sockets[0].getsockname()[1], host="127.0.0.1", idle_timeout=0.5
    )
    try:
        reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
        writer.write(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
        await writer.drain()
        got = read_frames(await asyncio.wait_for(reader.read(), 3))
        assert [f[3] for f in got if f[0] == 0x6] == [bytes([n]) * 8 for n in range(6)]
        writer.close()
    finally:
        await listener.stop()
        upstream.close()


async def test_start_serves_only_on_the_agents_port(monkeypatch):
    server = grpc.aio.server()
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    try:
        # No CODEFLY_CONNECT_PORT: the service does not set connect-endpoint, whatever
        # file is on disk, so nothing binds.
        monkeypatch.delenv(connect_gen.PORT_ENV, raising=False)
        assert await connect_gen.start(grpc_port, host="127.0.0.1") is None
        monkeypatch.setenv(connect_gen.PORT_ENV, "0")
        started = await connect_gen.start(grpc_port, host="127.0.0.1")
        assert started is not None
        try:
            async with httpx.AsyncClient() as client:
                r = await client.post(
                    f"http://127.0.0.1:{started.port}/echo.v1.Echo/Say",
                    json={"text": "s"},
                    headers={"content-type": "application/json", "x-caller": "zoe"},
                )
            assert r.json() == {"text": "s|zoe"}
        finally:
            await started.stop()
    finally:
        await server.stop(None)


@pytest.fixture
async def short_idle():
    """A face whose idle bound (0.5 s) is shorter than the slow call (2 s)."""
    server = grpc.aio.server(interceptors=(Proof(),))
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    listener = await connect_gen.serve(
        0, grpc_port, host="127.0.0.1", read_timeout=0.5, idle_timeout=0.5
    )
    try:
        yield listener
    finally:
        await listener.stop()
        await server.stop(None)


async def test_a_grpc_call_in_flight_outlives_the_idle_bound(short_idle):
    # The handler sends nothing for 2 s: an open call is never idle.
    async with grpc.aio.insecure_channel(f"127.0.0.1:{short_idle.port}") as channel:
        say_call = channel.unary_unary(
            "/echo.v1.Echo/Say",
            request_serializer=Msg.SerializeToString,
            response_deserializer=Msg.FromString,
        )
        answer = await say_call(Msg(text="slow"), metadata=tuple(STAMP.items()), timeout=10)
    assert answer.text == "slow|maya"


async def test_a_grpc_connection_with_no_open_call_is_closed(short_idle):
    async with grpc.aio.insecure_channel(f"127.0.0.1:{short_idle.port}") as channel:
        say_call = channel.unary_unary(
            "/echo.v1.Echo/Say",
            request_serializer=Msg.SerializeToString,
            response_deserializer=Msg.FromString,
        )
        assert (await say_call(Msg(text="q"), metadata=tuple(STAMP.items()))).text == "q|maya"
        # The call ended: the relayed connection is idle, and closed past the bound.
        for _ in range(30):
            if not short_idle._connections:
                break
            await asyncio.sleep(0.1)
        assert not short_idle._connections


PREFACE = b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"


def h2_frame(kind, flags, stream, payload=b""):
    size = len(payload)
    return struct.pack(">BHBBI", size >> 16, size & 0xFFFF, kind, flags, stream) + payload


SETTINGS = h2_frame(0x4, 0, 0)


def opens(stream):
    """A HEADERS frame opening ``stream`` (END_HEADERS; one indexed field)."""
    return h2_frame(0x1, 0x4, stream, b"\x82")


@pytest.fixture
async def silent_upstream():
    """A gRPC address that accepts, reads everything and never answers, so only the face's
    own rules end a connection relayed to it, and ``face(**options)`` serving over it.
    ``state["close_after"]`` makes it hang up after that many seconds instead, and
    ``state["mid_frame"]`` (an event) makes it send half a frame first and the rest when
    the event is set."""
    held, faces = [], []
    state = {"close_after": None, "received": bytearray(), "mid_frame": None}

    async def accept(reader, writer):
        held.append(writer)
        writer.write(SETTINGS)  # a server's connection preface
        if state["mid_frame"] is not None:
            # Half a PING, the rest once the test sets the event: the server is mid-frame.
            ping = h2_frame(0x6, 0, 0, b"12345678")
            writer.write(ping[:13])
            await writer.drain()

            async def finish():
                await state["mid_frame"].wait()
                writer.write(ping[13:])

            asyncio.ensure_future(finish())
        if state["close_after"] is not None:
            await asyncio.sleep(state["close_after"])
            writer.close()
            return
        while chunk := await reader.read(65536):
            state["received"] += chunk

    upstream = await asyncio.start_server(accept, "127.0.0.1", 0)
    port = upstream.sockets[0].getsockname()[1]

    async def face(**options):
        listener = await connect_gen.serve(0, port, host="127.0.0.1", **options)
        faces.append(listener)
        return listener

    try:
        yield face, state
    finally:
        for listener in faces:
            await listener.stop()
        for writer in held:
            writer.close()
        upstream.close()


async def closed_within(listener, payload, seconds):
    reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
    writer.write(payload)
    await writer.drain()
    try:
        async with asyncio.timeout(seconds):
            while await reader.read(65536):
                pass
        return True
    except TimeoutError:
        return False
    finally:
        writer.close()


def read_frames(data):
    """(type, flags, stream, payload) for each whole HTTP/2 frame in ``data``."""
    out = []
    while len(data) >= 9:
        high, low, kind, flags, stream = struct.unpack_from(">BHBBI", data)
        size = high << 16 | low
        out.append((kind, flags, stream & 0x7FFFFFFF, bytes(data[9 : 9 + size])))
        data = data[9 + size :]
    return out


async def exchange(listener, payload, seconds):
    """Send ``payload``; return whether the face closed within ``seconds``, and what it sent."""
    reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
    writer.write(payload)
    await writer.drain()
    got = bytearray()
    try:
        async with asyncio.timeout(seconds):
            while chunk := await reader.read(65536):
                got += chunk
        return True, got
    except TimeoutError:
        return False, got
    finally:
        writer.close()


async def test_a_client_opening_an_even_stream_gets_a_goaway(silent_upstream):
    # A client never opens an even stream; grpcio ignores one silently, so a face that
    # recorded it as a call would hold the connection, and its memory, forever. The
    # client learns why, and which calls the server has (1).
    face, _ = silent_upstream
    payload = PREFACE + SETTINGS + opens(1) + b"".join(opens(2 * n) for n in range(1, 1000))
    closed, got = await exchange(await face(), payload, 2)
    assert closed
    (goaway,) = [f for f in read_frames(got) if f[0] == 0x7]
    last, code = struct.unpack_from(">II", goaway[3])
    assert (last, code) == (1, 0x1) and b"stream 2" in goaway[3][8:]


async def test_a_call_past_the_limit_is_refused_alone(silent_upstream):
    # The server advertises no limit, so the face holds DEFAULT_MAX_OPEN_CALLS: the next
    # call gets ENHANCE_YOUR_CALM (not REFUSED_STREAM: the server receives its header
    # block, so it may start it), the server that block and CANCEL, and the connection
    # and its calls stay.
    face, state = silent_upstream
    calls = connect_gen.DEFAULT_MAX_OPEN_CALLS
    past = 2 * calls + 1
    payload = PREFACE + SETTINGS + b"".join(opens(2 * n + 1) for n in range(calls + 1))
    closed, got = await exchange(await face(), payload, 1)
    assert not closed
    assert [f for f in read_frames(got) if f[0] == 0x3] == [(0x3, 0, past, struct.pack(">I", 0xB))]
    upstream = read_frames(bytes(state["received"])[len(PREFACE) :])
    assert upstream[-2:] == [(0x1, 0x4, past, b"\x82"), (0x3, 0, past, struct.pack(">I", 0x8))]
    assert sum(1 for f in upstream if f[0] == 0x1) == calls + 1


async def test_a_refused_call_waits_for_the_end_of_its_header_block(silent_upstream):
    # A header block split into HEADERS and CONTINUATION is one unit: the reset follows
    # the CONTINUATION that ends it, never comes between.
    face, state = silent_upstream
    calls = connect_gen.DEFAULT_MAX_OPEN_CALLS
    past = 2 * calls + 1
    split = h2_frame(0x1, 0, past, b"") + h2_frame(0x9, 0x4, past, b"\x82")
    payload = PREFACE + SETTINGS + b"".join(opens(2 * n + 1) for n in range(calls)) + split
    closed, _ = await exchange(await face(), payload, 1)
    assert not closed
    upstream = read_frames(bytes(state["received"])[len(PREFACE) :])
    assert [f[0] for f in upstream[-3:]] == [0x1, 0x9, 0x3]


async def test_a_burst_past_the_limit_queues_and_loses_no_call(short_idle):
    # 1100 concurrent calls on one channel to a server advertising no limit: the face
    # advertises its own 1000, so the client queues the rest, and every call is answered.
    async with grpc.aio.insecure_channel(f"127.0.0.1:{short_idle.port}") as channel:
        say_call = channel.unary_unary(
            "/echo.v1.Echo/Say",
            request_serializer=Msg.SerializeToString,
            response_deserializer=Msg.FromString,
        )
        answers = await asyncio.gather(
            *[
                say_call(Msg(text="slow"), metadata=tuple(STAMP.items()), timeout=30)
                for _ in range(1100)
            ],
            return_exceptions=True,
        )
    failed = {str(a)[:200] for a in answers if isinstance(a, Exception)}
    assert not failed, failed
    assert all(a.text == "slow|maya" for a in answers)


def _settings(frame):
    """The (id, value) entries of a SETTINGS frame."""
    payload = frame[3]
    return [struct.unpack_from(">HI", payload, at) for at in range(0, len(payload), 6)]


async def _first_frames(upstream_preface, client_payload, monkeypatch=None):
    """What the client receives, and what the server receives, when a fake gRPC server
    sends ``upstream_preface`` 0.3 s after the connection and reads everything after."""
    received = bytearray()

    async def accept(reader, writer):
        await asyncio.sleep(0.3)
        writer.write(upstream_preface)
        await writer.drain()
        while chunk := await reader.read(65536):
            received.extend(chunk)

    upstream = await asyncio.start_server(accept, "127.0.0.1", 0)
    listener = await connect_gen.serve(0, upstream.sockets[0].getsockname()[1], host="127.0.0.1")
    try:
        closed, got = await exchange(listener, client_payload, 1)
        return read_frames(got), read_frames(bytes(received)[len(PREFACE) :])
    finally:
        await listener.stop()
        upstream.close()


async def test_a_server_advertising_no_limit_is_given_the_faces():
    # grpcio advertises no SETTINGS_MAX_CONCURRENT_STREAMS: the client is told the face's
    # DEFAULT_MAX_OPEN_CALLS in the server's own SETTINGS, so it queues within it.
    server_settings = h2_frame(0x4, 0, 0, struct.pack(">HI", 0x4, 65535))
    to_client, _ = await _first_frames(server_settings, PREFACE + SETTINGS)
    assert to_client[0][0] == 0x4
    assert _settings(to_client[0]) == [(0x4, 65535), (0x3, connect_gen.DEFAULT_MAX_OPEN_CALLS)]


async def test_a_server_limit_reaches_the_client_untouched():
    server_settings = h2_frame(0x4, 0, 0, struct.pack(">HI", 0x3, 7))
    to_client, _ = await _first_frames(server_settings, PREFACE + SETTINGS)
    assert _settings(to_client[0]) == [(0x3, 7)]


async def test_the_client_is_read_once_the_servers_limit_is_known(monkeypatch):
    # The client opens three calls at once; the server's SETTINGS (limit 2) arrives 0.3 s
    # later. The face waits for it, so the third call goes to the server, which refuses it
    # itself (retry-safe), rather than being reset by the face as past its own default.
    monkeypatch.setattr(connect_gen, "DEFAULT_MAX_OPEN_CALLS", 2)
    server_settings = h2_frame(0x4, 0, 0, struct.pack(">HI", 0x3, 2))
    to_client, to_server = await _first_frames(
        server_settings, PREFACE + SETTINGS + opens(1) + opens(3) + opens(5)
    )
    assert [f for f in to_client if f[0] == 0x3] == []
    assert [f[0] for f in to_server] == [0x4, 0x1, 0x1, 0x1]


def _literal_headers(headers):
    """An HPACK header block of literal fields without indexing (RFC 7541 6.2.2)."""
    block = bytearray()
    for name, value in headers:
        block += b"\x00" + bytes([len(name)]) + name.encode() + bytes([len(value)]) + value.encode()
    return bytes(block)


def _opens_chat(stream, n):
    """HEADERS opening an echo.v1.Echo/Chat call (bidi) numbered ``n``, body to come."""
    block = _literal_headers(
        [
            (":method", "POST"),
            (":scheme", "http"),
            (":path", "/echo.v1.Echo/Chat"),
            (":authority", "x"),
            ("content-type", "application/grpc"),
            ("te", "trailers"),
            ("x-n", str(n)),
        ]
    )
    return h2_frame(0x1, 0x4, stream, block)


async def _chat_server(entered, options=()):
    async def chat(requests, context):
        entered.add(int(dict(context.invocation_metadata())["x-n"]))
        await asyncio.sleep(30)
        yield Msg()

    codec = {"request_deserializer": Msg.FromString, "response_serializer": Msg.SerializeToString}
    server = grpc.aio.server(options=list(options))
    server.add_generic_rpc_handlers(
        (
            grpc.method_handlers_generic_handler(
                "echo.v1.Echo", {"Chat": grpc.stream_stream_rpc_method_handler(chat, **codec)}
            ),
        )
    )
    port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    return server, port


async def _past_the_limit(port, held, past):
    """Open ``held`` bidi calls, then ``past`` more; return the frames the client got."""
    reader, writer = await asyncio.open_connection("127.0.0.1", port)
    frames = FrameReader(reader, writer)
    try:
        writer.write(PREFACE + SETTINGS + b"".join(_opens_chat(2 * n + 1, n) for n in range(held)))
        await writer.drain()
        await asyncio.sleep(0.5)
        writer.write(b"".join(_opens_chat(2 * n + 1, n) for n in range(held, held + past)))
        await writer.drain()
        await asyncio.sleep(1)
        return frames.resets
    finally:
        frames.close()
        writer.close()


async def test_past_an_advertised_limit_only_the_server_refuses():
    # The server advertises 5: the sixth to eighth calls reach it and it refuses them
    # itself (REFUSED_STREAM, truthfully: their handlers never run). The face adds no
    # reset of its own.
    entered = set()
    server, port = await _chat_server(entered, [("grpc.max_concurrent_streams", 5)])
    listener = await connect_gen.serve(0, port, host="127.0.0.1")
    try:
        resets = await _past_the_limit(listener.port, 5, 3)
        assert entered == set(range(5))
        assert sorted(resets) == [(11, 0x7), (13, 0x7), (15, 0x7)]
    finally:
        await listener.stop()
        await server.stop(None)


async def test_past_the_faces_own_limit_a_call_is_never_said_unprocessed(monkeypatch):
    # The server advertises none, so the face holds DEFAULT_MAX_OPEN_CALLS (5 here). The
    # server receives a call past it (header compression needs the block) and may start
    # it, so the client is told ENHANCE_YOUR_CALM, never REFUSED_STREAM, which a gRPC
    # client retries on its own as "not processed".
    monkeypatch.setattr(connect_gen, "DEFAULT_MAX_OPEN_CALLS", 5)
    entered = set()
    server, port = await _chat_server(entered)
    listener = await connect_gen.serve(0, port, host="127.0.0.1")
    try:
        resets = await _past_the_limit(listener.port, 5, 3)
        assert {code for _, code in resets} == {0xB}, resets
        assert sorted(stream for stream, _ in resets) == [11, 13, 15]
        assert set(range(5)) <= entered
    finally:
        await listener.stop()
        await server.stop(None)


async def test_no_frame_is_written_inside_a_servers_header_block(monkeypatch):
    # The server's HEADERS without END_HEADERS, then a pause, then its CONTINUATION: a
    # reset for a call past the limit waits for the block's end (RFC 9113 6.10).
    monkeypatch.setattr(connect_gen, "DEFAULT_MAX_OPEN_CALLS", 2)
    go = asyncio.Event()

    async def accept(reader, writer):
        await reader.readexactly(len(PREFACE))
        writer.write(SETTINGS + h2_frame(0x1, 0, 1, b"\x88"))  # :status 200, block open
        await writer.drain()
        await go.wait()
        writer.write(h2_frame(0x9, 0x4, 1, b""))  # CONTINUATION ends the block
        await writer.drain()
        while await reader.read(65536):
            pass

    upstream = await asyncio.start_server(accept, "127.0.0.1", 0)
    listener = await connect_gen.serve(0, upstream.sockets[0].getsockname()[1], host="127.0.0.1")
    reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
    frames = FrameReader(reader)
    try:
        writer.write(PREFACE + SETTINGS + opens(1) + opens(3))
        await writer.drain()
        await asyncio.sleep(0.3)
        writer.write(opens(5))
        await writer.drain()
        await asyncio.sleep(0.3)
        assert frames.kinds[0x3] == 0, "a reset went out inside the header block"
        go.set()
        await frames.until(lambda: frames.kinds[0x3] == 1, 2)
        kinds = [f[0] for f in frames.frames] + [0x3]
        assert kinds == [0x4, 0x1, 0x9, 0x3]
    finally:
        frames.close()
        writer.close()
        await listener.stop()
        upstream.close()


async def test_a_server_limit_makes_a_burst_queue():
    # The scaffold's server advertises its limit; clients queue a burst behind it.
    server = grpc.aio.server(options=[("grpc.max_concurrent_streams", 10)])
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    listener = await connect_gen.serve(0, grpc_port, host="127.0.0.1")
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{listener.port}") as channel:
            say_call = channel.unary_unary(
                "/echo.v1.Echo/Say",
                request_serializer=Msg.SerializeToString,
                response_deserializer=Msg.FromString,
            )
            answers = await asyncio.gather(
                *[say_call(Msg(text=str(n)), timeout=30) for n in range(50)],
                return_exceptions=True,
            )
        assert [a.text for a in answers] == [f"{n}|" for n in range(50)]
    finally:
        await listener.stop()
        await server.stop(None)


async def test_a_stream_id_below_the_highest_opens_no_call(silent_upstream):
    # 1 and 99 are calls; 3 after 99 is not a new one. Once 1 and 99 are reset no call is
    # open, and the connection is closed past the idle bound.
    face, _ = silent_upstream
    reset = lambda stream: h2_frame(0x3, 0, stream, b"\x00\x00\x00\x08")  # noqa: E731
    payload = PREFACE + SETTINGS + opens(1) + opens(99) + opens(3) + reset(1) + reset(99)
    assert await closed_within(await face(idle_timeout=0.5), payload, 2)


async def test_the_relay_ends_when_the_grpc_server_hangs_up(silent_upstream):
    # A call is recorded open and the client is silent: the server closing its side
    # still ends the connection, rather than leaving the client's open.
    face, state = silent_upstream
    state["close_after"] = 0.3
    listener = await face()
    reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
    writer.write(PREFACE + SETTINGS + opens(1))
    await writer.drain()
    try:
        await asyncio.sleep(0.1)
        assert listener._connections, "the face took the connection"
        await asyncio.sleep(0.3)  # the server hangs up
        for _ in range(20):
            if not listener._connections:
                break
            await asyncio.sleep(0.1)
        assert not listener._connections, "the face still holds the client's connection"
        # The server's SETTINGS, then the end.
        assert [f[0] for f in read_frames(await asyncio.wait_for(reader.read(), 1))] == [0x4]
    finally:
        writer.close()


class FrameReader:
    """Reads a connection's frames as they come, counting each type; with ``writer``, it
    acknowledges the peer's SETTINGS as a client does."""

    def __init__(self, reader, writer=None):
        self._writer = writer
        self.kinds = collections.Counter()
        self.frames = []
        self.resets = []
        self.eof = False
        self._task = asyncio.ensure_future(self._read(reader))

    async def _read(self, reader):
        buffer = bytearray()
        while chunk := await reader.read(65536):
            buffer += chunk
            while len(buffer) >= 9:
                high, low, kind, flags, stream = struct.unpack_from(">BHBBI", buffer)
                size = high << 16 | low
                if len(buffer) < 9 + size:
                    break
                self.kinds[kind] += 1
                if kind == 0x4 and not flags & 0x1 and self._writer is not None:
                    self._writer.write(h2_frame(0x4, 0x1, 0))
                if kind == 0x3:
                    (code,) = struct.unpack_from(">I", buffer, 9)
                    self.resets.append((stream & 0x7FFFFFFF, code))
                else:
                    self.frames.append((kind, flags, stream, bytes(buffer[9 : 9 + size])))
                del buffer[: 9 + size]
        self.eof = True

    async def until(self, condition, seconds):
        async with asyncio.timeout(seconds):
            while not condition():
                await asyncio.sleep(0.02)

    def close(self):
        self._task.cancel()


async def test_a_reading_client_past_the_limit_keeps_its_calls(silent_upstream):
    # The server is mid-frame, so every refusal waits for that frame's end: far more of
    # them than the face holds. A client that reads keeps its connection and its calls,
    # and gets every refusal once the frame completes.
    face, state = silent_upstream
    state["mid_frame"] = asyncio.Event()
    listener = await face()
    calls, past = connect_gen.DEFAULT_MAX_OPEN_CALLS, 3000
    reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
    frames = FrameReader(reader)
    try:
        writer.write(PREFACE + SETTINGS + b"".join(opens(2 * n + 1) for n in range(calls)))
        await writer.drain()
        await asyncio.sleep(0.2)
        writer.write(b"".join(opens(2 * n + 1) for n in range(calls, calls + past)))
        await writer.drain()
        await asyncio.sleep(0.3)
        assert not frames.eof and listener._connections
        state["mid_frame"].set()
        await frames.until(lambda: frames.kinds[0x3] == past, 5)
        assert not frames.eof and listener._connections
        upstream = read_frames(bytes(state["received"])[len(PREFACE) :])
        assert sum(1 for f in upstream if f[0] == 0x1) == calls + past
    finally:
        frames.close()
        writer.close()


async def test_a_client_that_reads_nothing_is_not_read(silent_upstream):
    # The refusals cannot be written (the server stays mid-frame): past what the face
    # holds it stops reading the client, so the client's own writes back up and the
    # face's memory does not grow with them.
    face, state = silent_upstream
    state["mid_frame"] = asyncio.Event()
    listener = await face()
    calls = connect_gen.DEFAULT_MAX_OPEN_CALLS
    reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
    try:
        writer.write(PREFACE + SETTINGS + b"".join(opens(2 * n + 1) for n in range(calls)))
        await writer.drain()
        await asyncio.sleep(0.2)
        flood = b"".join(opens(2 * n + 1) for n in range(calls, calls + 1_000_000))
        writer.write(flood)
        with pytest.raises(TimeoutError):
            await asyncio.wait_for(writer.drain(), 2)
        forwarded = read_frames(bytes(state["received"])[len(PREFACE) :])
        assert sum(1 for f in forwarded if f[0] == 0x1) < calls + 20_000
        assert listener._connections
    finally:
        writer.transport.abort()


async def test_the_even_stream_goaway_waits_for_the_servers_frame(silent_upstream):
    face, state = silent_upstream
    state["mid_frame"] = asyncio.Event()
    listener = await face()
    reader, writer = await asyncio.open_connection("127.0.0.1", listener.port)
    frames = FrameReader(reader)
    try:
        writer.write(PREFACE + SETTINGS + opens(1))
        await writer.drain()
        await asyncio.sleep(0.2)
        writer.write(opens(2))
        await writer.drain()
        await asyncio.sleep(0.2)
        state["mid_frame"].set()
        await frames.until(lambda: frames.eof, 3)
        kinds = [f[0] for f in frames.frames]
        assert kinds == [0x4, 0x6, 0x7]  # the server's frame whole, then the GOAWAY
        last, code = struct.unpack_from(">II", frames.frames[-1][3])
        assert (last, code) == (1, 0x1)
    finally:
        frames.close()
        writer.close()


async def test_at_the_scaffolds_limit_a_burst_fails_only_retry_safely():
    # The scaffold advertises 1000: most of a 1200-call burst queues in the client, and
    # any call grpcio refuses comes back REFUSED_STREAM, never processed.
    server = grpc.aio.server(options=[("grpc.max_concurrent_streams", 1000)])
    server.add_generic_rpc_handlers((_handler(),))
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    listener = await connect_gen.serve(0, grpc_port, host="127.0.0.1")
    try:
        async with grpc.aio.insecure_channel(f"127.0.0.1:{listener.port}") as channel:
            say_call = channel.unary_unary(
                "/echo.v1.Echo/Say",
                request_serializer=Msg.SerializeToString,
                response_deserializer=Msg.FromString,
            )
            answers = await asyncio.gather(
                *[say_call(Msg(text=str(n)), timeout=30) for n in range(1200)],
                return_exceptions=True,
            )
        refused = [a for a in answers if isinstance(a, Exception)]
        assert all("error code 7" in a.details() for a in refused), {
            a.details() for a in refused
        }
        assert len(refused) < 600
    finally:
        await listener.stop()
        await server.stop(None)
