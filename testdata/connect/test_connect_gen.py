"""The generated Connect face (templates/connect/connect_gen.py) against a real grpc.aio
server: every protocol it serves answers through that server, interceptors included."""

import asyncio
import base64
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


async def say(request, context):
    metadata = dict(context.invocation_metadata())
    if request.text == "deny":
        context.set_trailing_metadata((("model-error", '{"code":"Denied"}'),))
        await context.abort(grpc.StatusCode.PERMISSION_DENIED, "denied: not yours")
    if request.text == "slow":
        await asyncio.sleep(2)
    await context.send_initial_metadata((("x-answer", "header"),))
    context.set_trailing_metadata((("x-trail", "trailer"), ("x-raw-bin", b"\x00\xff")))
    return Msg(text=f"{request.text}|{metadata.get('x-caller', '')}", count=request.count)


async def count(request, context):
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
        for _ in range(6):
            writer.write(b".")
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
        assert await asyncio.wait_for(reader.read(), 3) == b"......"
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
    ``state["close_after"]`` makes it hang up after that many seconds instead."""
    held, faces = [], []
    state = {"close_after": None}

    async def accept(reader, writer):
        held.append(writer)
        if state["close_after"] is not None:
            await asyncio.sleep(state["close_after"])
            writer.close()
            return
        while await reader.read(65536):
            pass

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


async def test_a_client_opening_an_even_stream_is_closed(silent_upstream):
    # A client never opens an even stream; grpcio ignores one silently, so a face that
    # recorded it as a call would hold the connection, and its memory, forever.
    face, _ = silent_upstream
    payload = PREFACE + SETTINGS + b"".join(opens(2 * n) for n in range(1, 1000))
    assert await closed_within(await face(), payload, 2)


async def test_open_calls_on_one_connection_are_bounded(silent_upstream):
    face, _ = silent_upstream
    listener = await face()
    calls = connect_gen.DEFAULT_MAX_OPEN_CALLS
    within = PREFACE + SETTINGS + b"".join(opens(2 * n + 1) for n in range(calls))
    assert not await closed_within(listener, within, 1)
    beyond = PREFACE + SETTINGS + b"".join(opens(2 * n + 1) for n in range(calls + 1))
    assert await closed_within(listener, beyond, 2)


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
        assert await asyncio.wait_for(reader.read(), 1) == b""
    finally:
        writer.close()
