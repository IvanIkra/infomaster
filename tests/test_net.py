"""D79: the service fetches what other people link to from public addresses only."""

import pytest
from aiohttp import web

from infomaster.collectors import links
from infomaster.net import PrivateAddress, is_public, public_session


def test_public_addresses():
    for address in ("8.8.8.8", "2a00:1450:4010:c05::64", "1.1.1.1"):
        assert is_public(address), address
    for address in ("127.0.0.1", "10.0.0.5", "172.30.77.10", "192.168.1.1", "169.254.169.254", "100.100.100.100",
                    "::1", "fe80::1", "fd7a:115c:a1e0::1", "::ffff:127.0.0.1", "2002:7f00:1::", "0.0.0.0",
                    "224.0.0.1", "not an address"):
        assert not is_public(address), address


async def test_a_local_server_is_never_reached(aiohttp_server, tmp_path):
    asked = []

    async def pdf(request):
        asked.append(request.path)
        return web.Response(body=b"%PDF-1.4 secret")

    app = web.Application()
    app.router.add_get("/{name}", pdf)
    server = await aiohttp_server(app)
    with pytest.raises(PrivateAddress):
        async with public_session() as session:
            await session.get(f"http://127.0.0.1:{server.port}/x.pdf")
    with pytest.raises(PrivateAddress):
        async with public_session() as session:
            await session.get(f"http://localhost:{server.port}/x.pdf")
    # A chat's link into the service's own network is no sheet, and is not tried again.
    assert await links.fetch(f"http://127.0.0.1:{server.port}/notes.pdf", tmp_path / "f.pdf", 10**6) is None
    assert asked == []
