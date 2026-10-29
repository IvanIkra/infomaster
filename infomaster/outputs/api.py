"""Internal API for Tablo (§8.1, §8.2): its own port, never behind Funnel, Bearer token on /api/.

GET /api/deadlines: {"items": [{uid, title, due, category, source, overdue}]}, the merged list in screen order, as
Tablo's route gave it (§8). Own and hidden deadlines are kept in the Mini App and with the forum's buttons: HA's
to-do lists are gone (D34), and with them the routes that changed the list.

GET /api/news: the live news (§8.3, D9, D23), the items of the «Новости» topic and the same rules, in rank order:
{"items": [{id, source, study, posted_at, full, short, importance}], "ttl_hours"}. Replaced, withdrawn and hidden items
are not given. This is the level Б of §11.4: Tablo only lays them out in its column (§6.6).

GET /api/grades: the new grades of the window (§6.7), {"items": [{key, label, value, bonus, seen_at}], "window_days"},
newest first: the newest event of each work. Tablo had no route for them (§8): this is the level В of §11.5.

The iCal feeds live here too, under /calendar/<secret>/ without the token (see ical_feed).
"""

import datetime as dt
import hmac
import json
from collections.abc import Callable
from functools import partial

from aiohttp import web

from .. import clock
from ..config import Env
from ..i18n import json_default
from ..llm.budget import Budget
from ..processing.grades import Grades
from ..processing.merge import DeadlineList
from ..processing.news import News
from ..settings import Settings
from . import ical_feed

ENV_KEY = web.AppKey("env", Env)
BUDGET_KEY = web.AppKey("budget", Budget)
SOURCES_KEY = web.AppKey("sources", dict)
DEADLINES_KEY = web.AppKey("deadlines", DeadlineList)
GRADES_KEY = web.AppKey("grades", Grades)
NEWS_KEY = web.AppKey("news", News)
SETTINGS_KEY = web.AppKey("settings", Settings)
NOW_KEY = web.AppKey("now", Callable[[], dt.datetime])
NEWS_FIELDS = ("id", "source", "study", "posted_at", "full", "short", "importance")

Sources = dict[str, Callable[[], dict]]  # source name -> its status: ok_at, error and its own flags


@web.middleware
async def bearer(request: web.Request, handler):
    if request.path.startswith("/api/"):
        scheme, _, token = request.headers.get("Authorization", "").partition(" ")
        if scheme != "Bearer" or not hmac.compare_digest(token.encode(), request.app[ENV_KEY].api_token.encode()):
            raise web.HTTPUnauthorized(text="Bearer token required")
    return await handler(request)


async def status(request: web.Request) -> web.Response:
    """Every source that has joined so far, and the LLM spending of today. An error kept as a Phrase (D41) is
    given in English."""
    return web.json_response({**{name: status() for name, status in request.app[SOURCES_KEY].items()},
                              "llm": request.app[BUDGET_KEY].status()},
                             dumps=partial(json.dumps, default=json_default("en")))


async def list_deadlines(request: web.Request) -> web.Response:
    return web.json_response({"items": request.app[DEADLINES_KEY].items()})


async def news(request: web.Request) -> web.Response:
    ttl_hours = request.app[SETTINGS_KEY].get("news_ttl_hours")
    items = request.app[NEWS_KEY].live(request.app[NOW_KEY](), ttl_hours)
    return web.json_response({"items": [{name: item[name] for name in NEWS_FIELDS} for item in items],
                              "ttl_hours": ttl_hours})


async def grades(request: web.Request) -> web.Response:
    source = request.app[GRADES_KEY]
    return web.json_response({"items": source.new(), "window_days": source.settings.get("grades_window_days")})


def create_app(env: Env, budget: Budget, sources: Sources, calendar: ical_feed.CalendarFeed,
               deadlines: DeadlineList, grade_list: Grades | None = None, news_list: News | None = None,
               settings: Settings | None = None, now: Callable[[], dt.datetime] = clock.now_utc) -> web.Application:
    app = web.Application(middlewares=[bearer])
    app[ENV_KEY], app[BUDGET_KEY], app[SOURCES_KEY], app[DEADLINES_KEY] = env, budget, sources, deadlines
    app[NOW_KEY] = now
    if grade_list is not None:
        app[GRADES_KEY] = grade_list
        app.router.add_get("/api/grades", grades)
    if news_list is not None and settings is not None:
        app[NEWS_KEY], app[SETTINGS_KEY] = news_list, settings
        app.router.add_get("/api/news", news)
    app.router.add_get("/api/status", status)
    app.router.add_get("/api/deadlines", list_deadlines)
    ical_feed.add_routes(app, env.calendar_secret, calendar)
    return app
