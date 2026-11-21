"""Outbound HTTP to addresses on the public internet only (D79).

Links in chats and letters, wiki pages and grade sheets name hosts that other people choose. A request to such a host
must not reach the service's neighbours: the docker network (Tablo, the internal port), the host, the tailnet, a
cloud's metadata address. `public_session` resolves every host itself and connects only to global addresses, on
every redirect too, and by the address it checked, so a name that resolves anew (DNS rebinding) changes nothing.
"""

import ipaddress
from collections.abc import Sequence

import aiohttp
from aiohttp.abc import ResolveResult


class PrivateAddress(aiohttp.ClientConnectionError):
    """The host is not on the public internet."""


def is_public(address: str) -> bool:
    try:
        ip = ipaddress.ip_address(address.split("%", 1)[0])
    except ValueError:
        return False
    if isinstance(ip, ipaddress.IPv6Address) and (mapped := ip.ipv4_mapped or ip.sixtofour) is not None:
        ip = mapped
    return ip.is_global and not ip.is_multicast


class PublicConnector(aiohttp.TCPConnector):
    async def _resolve_host(self, host: str, port: int, traces: Sequence | None = None) -> list[ResolveResult]:
        found = await super()._resolve_host(host, port, traces)
        public = [entry for entry in found if is_public(entry["host"])]
        if not public:
            raise PrivateAddress(f"{host} is not a public address")
        return public


def public_session(**kwargs) -> aiohttp.ClientSession:
    """A ClientSession that connects to public addresses only."""
    return aiohttp.ClientSession(connector=PublicConnector(), **kwargs)
