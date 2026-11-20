"""The MCP connector (§8.5, D57): infomaster as a remote MCP server that claude.ai adds as a custom connector.

On the public port, next to the web app:
- /mcp: the endpoint (mcp.py), behind an OAuth access token;
- /.well-known/oauth-*, /oauth/*: the authorization server for the one owner (oauth.py).
The web app's «Machine» shows the address to paste into claude.ai, the connections with Revoke and the journal of
changes with Undo (changes.py), from `state`.
"""

import datetime as dt
import time
from collections.abc import Awaitable, Callable
from urllib.parse import urlsplit

from aiohttp import web

from .. import clock
from ..config import Env
from ..library.links import LIBRARY_KEY
from ..processing.news import News
from ..store import Store
from . import catalog, oauth
from . import library as library_tools
from .changes import Changes
from .mcp import Endpoint
from .tools import Context, Output, Registry, ToolError, check


class Connector:
    def __init__(self, env: Env, store: Store, news: News,
                 send_code: Callable[[str], Awaitable[None]] | None = None,
                 lang: Callable[[], str] = lambda: "en", now: Callable[[], dt.datetime] = clock.now_utc,
                 seconds: Callable[[], float] = time.time):
        self.env, self.store, self.news, self.now = env, store, news, now
        parts = urlsplit(env.public_url)
        self.public_origin = f"{parts.scheme}://{parts.netloc}"
        self.url = env.public_url + "/mcp"
        self.oauth = oauth.OAuth(store, env.public_url, send_code, lang, seconds)
        self.changes = Changes(store, now)
        self.registry = Registry()
        self.endpoint = Endpoint(self.registry, self, self._record)
        self.app: web.Application | None = None

    def attach(self, app: web.Application) -> web.Application:
        """The connector's routes and tools on the public port's app (actions.public_app): the tools act on the
        services it holds."""
        self.app = app
        catalog.register(self.registry, app)
        if app.get(LIBRARY_KEY) is not None:  # put there by library.links.add_routes, before this
            library_tools.register(self.registry, app)
        oauth.add_routes(app, self.oauth)
        app[CONNECTOR_KEY] = self
        app.router.add_route("*", "/mcp", self._mcp)
        return app

    async def _mcp(self, request: web.Request) -> web.Response:
        scheme, _, token = request.headers.get("Authorization", "").partition(" ")
        grant_id = self.oauth.check(token) if scheme.lower() == "bearer" and token else None
        if grant_id is None:
            challenge = f'Bearer resource_metadata="{self.oauth.resource_metadata}"'
            if token:
                challenge += ', error="invalid_token", error_description="the token is unknown, revoked or expired"'
            return web.json_response({"error": "invalid_token" if token else "unauthorized"}, status=401,
                                     headers={"WWW-Authenticate": challenge})
        if request.method != "POST":  # no stream to open, no session to end
            return web.Response(status=405, headers={"Allow": "POST"})
        return await self.endpoint.post(request, grant_id)

    def _record(self, grant_id: int | None, tool: str, arguments: dict, output: Output) -> int:
        return self.changes.record(grant_id, tool, arguments, output.summary, output.undo, output.note)

    async def act(self, tool: str, arguments: dict, via: str = "web") -> tuple[int, Output]:
        """A write tool called by the owner elsewhere (the web app, §8.6): checked like a client's call and journaled
        with its undo. ToolError when the tool refuses."""
        found = self.registry.tools.get(tool)
        if found is None or not found.write:
            raise ToolError(f"no such change: {tool}")
        if problems := check(found.schema, arguments):
            raise ToolError("; ".join(problems))
        output = await found.run(Context(self.app, self), arguments)
        if not isinstance(output, Output):
            output = Output(output)
        return self.changes.record(None, tool, arguments, output.summary, output.undo, output.note, via), output

    def journal(self, tool: str, arguments: dict, summary: str, undo: list[dict] | None,
                note: str = "", via: str = "web") -> int:
        """A change made without a tool (a question of the web app's «Check» put aside), for its undo."""
        return self.changes.record(None, tool, arguments, summary, undo, note, via)

    async def undo(self, change_id: int) -> dict:
        """Run a change's undo steps; ToolError with the reason when it cannot be done."""
        change = self.changes.get(change_id)
        if change is None:
            raise ToolError("no such change")
        if change["undone_at"] is not None:
            raise ToolError("it is undone already")
        if change["undo"] is None:
            raise ToolError(change["note"] or "it cannot be undone")
        context = Context(self.app, self)
        try:
            for step in change["undo"]:
                await self.registry.step(context, step)
        except ToolError as e:
            self.changes.undone(change_id, str(e))
            raise
        self.changes.undone(change_id, None)
        return self.changes.get(change_id)

    def state(self) -> dict:
        def local(seconds: int | None) -> str | None:
            return clock.local_iso(dt.datetime.fromtimestamp(seconds, dt.UTC)) if seconds else None

        tools = self.registry.tools.values()
        return {"url": self.url,
                "connections": [{**c, "created_at": local(c["created_at"]), "used_at": local(c["used_at"])}
                                for c in self.oauth.connections()],
                "changes": [{"id": c["id"], "at": c["at"], "tool": c["tool"], "summary": c["summary"],
                             "undo": c["undo"] is not None, "note": c["note"], "undone_at": c["undone_at"],
                             "undo_error": c["undo_error"]} for c in self.changes.recent(via="claude")],
                "tools": {"read": sum(not t.write for t in tools), "write": sum(t.write for t in tools)}}


CONNECTOR_KEY = web.AppKey("connector", Connector)

