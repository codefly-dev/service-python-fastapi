"""A request body the service's proto refuses is the caller's invalid argument.

The generated Connect face (templates/connect/connect_gen.py) answers it as the protocol's
own error, on every protocol and codec it transcodes, and the service's handler never
runs. The fixture contract says required both ways a proto can: a proto2 ``required``
member and an edition 2023 member with ``features.field_presence = LEGACY_REQUIRED``."""

import json
import struct

import connect_gen
import grpc
import httpx
import pytest
from google.protobuf import descriptor_pb2, descriptor_pool, message_factory

ENVELOPE = struct.Struct(">BI")
STRING, INT32, ENUM = 9, 5, 14
OPTIONAL, REQUIRED = 1, 2


def _register(package, edition):
    """One contract: Program{code required, kind, count}, Run (unary), Follow (stream)."""
    file = descriptor_pb2.FileDescriptorProto(
        name=f"codefly_connect_test/{package}.proto", package=package
    )
    if edition:
        file.syntax, file.edition = "editions", descriptor_pb2.EDITION_2023
    else:
        file.syntax = "proto2"
    kind = file.enum_type.add(name="Kind")
    kind.value.add(name="KIND_PLAIN", number=0)
    kind.value.add(name="KIND_FAST", number=1)
    program = file.message_type.add(name="Program")
    code = program.field.add(
        name="code", number=1, type=STRING, label=OPTIONAL if edition else REQUIRED
    )
    if edition:
        code.options.features.field_presence = descriptor_pb2.FeatureSet.LEGACY_REQUIRED
    program.field.add(
        name="kind", number=2, type=ENUM, type_name=f".{package}.Kind", label=OPTIONAL
    )
    program.field.add(name="count", number=3, type=INT32, label=OPTIONAL)
    out = file.message_type.add(name="Out")
    out.field.add(name="text", number=1, type=STRING, label=OPTIONAL)
    service = file.service.add(name="Strict")
    for name, streams in (("Run", False), ("Follow", True)):
        service.method.add(
            name=name,
            input_type=f".{package}.Program",
            output_type=f".{package}.Out",
            server_streaming=streams,
        )
    pool = descriptor_pool.Default()
    pool.AddSerializedFile(file.SerializeToString())
    return tuple(
        message_factory.GetMessageClass(pool.FindMessageTypeByName(f"{package}.{name}"))
        for name in ("Program", "Out")
    )


CONTRACTS = {
    "strict.proto2": _register("strict.proto2", edition=False),
    "strict.edition2023": _register("strict.edition2023", edition=True),
}


@pytest.fixture
async def served():
    """The face over a server whose handlers record every program they are given."""
    ran = []
    server = grpc.aio.server()
    for package, (program, out) in CONTRACTS.items():

        async def run(request, context, out=out):
            ran.append(request)
            return out(text=request.code)

        async def follow(request, context, out=out):
            ran.append(request)
            yield out(text=request.code)

        codec = {
            "request_deserializer": program.FromString,
            "response_serializer": out.SerializeToString,
        }
        server.add_generic_rpc_handlers(
            (
                grpc.method_handlers_generic_handler(
                    f"{package}.Strict",
                    {
                        "Run": grpc.unary_unary_rpc_method_handler(run, **codec),
                        "Follow": grpc.unary_stream_rpc_method_handler(follow, **codec),
                    },
                ),
            )
        )
    grpc_port = server.add_insecure_port("127.0.0.1:0")
    await server.start()
    face = await connect_gen.serve(0, grpc_port, host="127.0.0.1", read_timeout=2)
    try:
        yield face, ran
    finally:
        await face.stop()
        await server.stop(None)


def _frames(body):
    out = []
    while body:
        flags, size = ENVELOPE.unpack_from(body)
        out.append((flags, body[ENVELOPE.size : ENVELOPE.size + size]))
        body = body[ENVELOPE.size + size :]
    return out


async def _call(face, package, method, media, body):
    """Post one body; return the protocol's error as (code, message), or None if the
    call was answered."""
    enveloped = "connect+" in media or "grpc-web" in media
    content = ENVELOPE.pack(0, len(body)) + body if enveloped else body
    async with httpx.AsyncClient(trust_env=False) as client:
        response = await client.post(
            f"http://127.0.0.1:{face.port}/{package}.Strict/{method}",
            content=content,
            headers={"content-type": media},
        )
    if not enveloped:
        if response.status_code == 200:
            return None
        document = response.json()
        assert response.status_code == 400, response.text
        return document["code"], document.get("message", "")
    assert response.status_code == 200, response.text
    flags, end = _frames(response.content)[-1]
    if media.startswith("application/grpc-web"):
        assert flags == 0x80
        trailers = dict(line.split(": ", 1) for line in end.decode().strip().split("\r\n"))
        if trailers["grpc-status"] == "0":
            return None
        code = {"3": "invalid_argument"}.get(trailers["grpc-status"], trailers["grpc-status"])
        return code, trailers.get("grpc-message", "")
    assert flags == 0x02
    error = json.loads(end).get("error")
    return None if error is None else (error["code"], error.get("message", ""))


# Every (method, content type) the face transcodes, by codec.
JSON_CALLS = [
    ("Run", "application/json"),
    ("Run", "application/connect+json"),
    ("Run", "application/grpc-web+json"),
    ("Follow", "application/connect+json"),
    ("Follow", "application/grpc-web+json"),
]
PROTO_CALLS = [
    ("Run", "application/proto"),
    ("Run", "application/connect+proto"),
    ("Run", "application/grpc-web+proto"),
    ("Follow", "application/connect+proto"),
    ("Follow", "application/grpc-web+proto"),
]


@pytest.mark.parametrize("package", list(CONTRACTS))
@pytest.mark.parametrize("method,media", JSON_CALLS)
@pytest.mark.parametrize("body", [b"{}", b'{"code":null}', b'{"count":3}', b""])
async def test_json_without_a_required_member_names_it(served, package, method, media, body):
    face, ran = served
    code, message = await _call(face, package, method, media, body)
    assert code == "invalid_argument"
    assert "missing required" in message and "code" in message, message
    assert ran == []


@pytest.mark.parametrize("package", list(CONTRACTS))
@pytest.mark.parametrize("method,media", PROTO_CALLS)
@pytest.mark.parametrize("body", [b"", b"\x18\x03"], ids=["empty", "count-only"])
async def test_proto_without_a_required_member_names_it(served, package, method, media, body):
    face, ran = served
    code, message = await _call(face, package, method, media, body)
    assert code == "invalid_argument"
    assert "missing required" in message and "code" in message, message
    assert ran == []


@pytest.mark.parametrize("package", list(CONTRACTS))
@pytest.mark.parametrize("method,media", PROTO_CALLS)
async def test_proto_that_does_not_parse_is_invalid(served, package, method, media):
    face, ran = served
    code, message = await _call(face, package, method, media, b"\xff\xff")
    assert code == "invalid_argument"
    assert "unmarshal" in message, message
    assert ran == []


@pytest.mark.parametrize("package", list(CONTRACTS))
@pytest.mark.parametrize("method,media", JSON_CALLS)
@pytest.mark.parametrize(
    "body,named",
    [
        (b'{"code":5}', "code"),  # a number where the member is a string
        (b'{"code":"x","count":"abc"}', "count"),
        (b'{"code":"x","kind":{}}', "kind"),
        (b'{"code":', ""),  # not JSON at all
        (b"3", ""),  # JSON, but no message
        (b"\xff", ""),  # not text at all
    ],
)
async def test_json_of_the_wrong_shape_is_invalid(served, package, method, media, body, named):
    face, ran = served
    code, message = await _call(face, package, method, media, body)
    assert code == "invalid_argument"
    assert "unmarshal JSON" in message and named in message, message
    assert ran == []


@pytest.mark.parametrize("method,media", JSON_CALLS)
async def test_a_number_outside_a_closed_enum_is_invalid(served, method, media):
    """proto2's enum is closed: a number it does not declare is no value of it. An
    edition 2023 enum is open by default, and takes the number."""
    face, ran = served
    code, message = await _call(face, "strict.proto2", method, media, b'{"code":"x","kind":7}')
    assert code == "invalid_argument" and "kind" in message, message
    assert ran == []
    assert await _call(face, "strict.edition2023", method, media, b'{"code":"x","kind":7}') is None
    assert [request.kind for request in ran] == [7]


@pytest.mark.parametrize("package", list(CONTRACTS))
async def test_an_unknown_enum_name_is_dropped_as_an_unknown_field_is(served, package):
    """What the face already promises of a client built from a newer contract, and what
    connect-go's JSON codec does (DiscardUnknown): the name is ignored, the member unset."""
    face, ran = served
    body = b'{"code":"x","kind":"KIND_FROM_A_NEWER_CONTRACT"}'
    assert await _call(face, package, "Run", "application/json", body) is None
    assert [(request.code, request.HasField("kind")) for request in ran] == [("x", False)]


@pytest.mark.parametrize("package", list(CONTRACTS))
@pytest.mark.parametrize(
    "method,media,body",
    [(method, media, b'{"code":"print(1)","kind":"KIND_FAST"}') for method, media in JSON_CALLS]
    + [(method, media, b"\x0a\x08print(1)\x10\x01") for method, media in PROTO_CALLS],
)
async def test_a_whole_program_runs_once(served, package, method, media, body):
    face, ran = served
    assert await _call(face, package, method, media, body) is None
    assert [(request.code, request.kind) for request in ran] == [("print(1)", 1)]
