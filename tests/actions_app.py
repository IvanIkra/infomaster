"""The owner's actions (infomaster/actions.py) over HTTP, for the tests only: the service calls them through the
connector's tools and the web app, never by these paths. Each handler is mounted where the tests ask for it."""

from aiohttp import web

from infomaster import actions

ROUTES = [
    ("GET", "/chats", actions.chats), ("PATCH", "/chats/{chat_id}", actions.update_chat),
    ("GET", "/chats/{chat_id}/senders", actions.senders),
    ("POST", "/twin/login", actions.twin_login), ("POST", "/twin/cancel", actions.twin_cancel),
    ("POST", "/twin/logout", actions.twin_logout),
    ("GET", "/llm", actions.llm), ("PATCH", "/llm", actions.update_llm), ("POST", "/llm/raise", actions.raise_llm),
    ("PATCH", "/images", actions.update_images),
    ("GET", "/general", actions.general), ("PATCH", "/general", actions.update_general),
    ("GET", "/subjects", actions.subjects), ("PUT", "/subjects", actions.put_subject),
    ("DELETE", "/subjects", actions.delete_subject),
    ("GET", "/agenda", actions.agenda), ("GET", "/message", actions.message),
    ("POST", "/events", actions.add_event), ("PATCH", "/events/{event_id}", actions.update_event),
    ("DELETE", "/events/{event_id}", actions.delete_event), ("POST", "/corrections", actions.correct_class),
    ("GET", "/deadlines", actions.deadlines), ("POST", "/deadlines", actions.add_deadline),
    ("PATCH", "/deadlines/{uid}", actions.edit_deadline), ("DELETE", "/deadlines/{uid}", actions.delete_deadline),
    ("DELETE", "/hidden/{uid}", actions.unhide_deadline),
    ("GET", "/sheets", actions.sheets), ("PATCH", "/sheets", actions.update_sheets),
    ("POST", "/sheets", actions.add_sheet), ("PATCH", "/sheets/{sheet_id}", actions.edit_sheet),
    ("DELETE", "/sheets/{sheet_id}", actions.delete_sheet), ("POST", "/sheets/{sheet_id}/join", actions.join_sheet),
    ("GET", "/reminders", actions.reminders), ("PATCH", "/reminders", actions.update_reminders),
    ("GET", "/grades", actions.grade_sheets), ("PATCH", "/grades", actions.update_grades),
    ("POST", "/grades/items", actions.add_grade), ("PATCH", "/grades/items/{grade}", actions.change_grade),
    ("DELETE", "/grades/items/{grade}", actions.drop_grade),
    ("GET", "/grades/sheets/{sid}", actions.gradebook), ("PATCH", "/grades/sheets/{sid}", actions.update_gradebook),
    ("POST", "/grades/sheets/{sid}/read", actions.read_gradebook),
    ("PUT", "/grades/sheets/{sid}/tabs/{tab}", actions.set_layout),
    ("DELETE", "/grades/sheets/{sid}/tabs/{tab}", actions.clear_layout),
    ("GET", "/hseapp", actions.hseapp), ("PATCH", "/hseapp", actions.update_hseapp),
    ("POST", "/hseapp/link", actions.hseapp_link), ("POST", "/hseapp/login", actions.hseapp_login),
]
PREFIX = "/app/api"


def mounted(app: web.Application) -> web.Application:
    """actions.public_app with every action at PREFIX + its path."""
    for method, path, handler in ROUTES:
        app.router.add_route(method, PREFIX + path, handler)
    return app


def app(*args, **kwargs) -> web.Application:
    """actions.public_app(...) with the actions mounted; the old keyword `sheet_topic` is `sheets`."""
    if "sheet_topic" in kwargs:
        kwargs["sheets"] = kwargs.pop("sheet_topic")
    return mounted(actions.public_app(*args, **kwargs))
