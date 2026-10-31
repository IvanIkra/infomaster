"""One deadline list from all sources (§5.5, §5.6): duplicates, work completed in HSE App X, hidden and own ones."""

import datetime as dt
import hashlib

import pytest

from infomaster.processing import merge
from infomaster.processing.merge import drop_done, drop_hidden, drop_moved, merge_deadlines, visible
from infomaster.processing.todo import Deadline, uid

D = dt.date
TODAY = D(2026, 10, 20)  # the `now` fixture: Tue noon in Moscow
CHAT = -1001


def dl(title, due, source="mail", course=None):
    return Deadline(title, due, "study", source, course)


# --- Tablo's rules, case by case (its tests/test_deadlines.py) -------------------------------

def test_completed_in_hseapp_hides_copy_with_extended_date():
    done = (dl("Safety HW 2", D(2026, 10, 13), "hseapp"),)  # the app keeps the first date
    mail = dl("Safety HW 2", D(2026, 10, 15), "mail")  # the letter carries the extension
    assert drop_done((mail,), done) == ()


def test_completed_matches_by_course_and_work():
    done = (dl("DM HW 3", D(2026, 10, 15), "hseapp", course="DM"),)
    assert drop_done((dl("DM HW 3 late", D(2026, 10, 18), "tg", course="DM"),), done) == ()
    kept = (dl("DM HW 30", D(2026, 10, 18), "tg", course="DM"),  # another work
            dl("DM HW 3B", D(2026, 10, 18), "tg", course="DM"),
            dl("Calc HW 3", D(2026, 10, 15), "tg", course="Calc"),  # another course
            dl("DM HW 3", D(2026, 11, 2), "tg", course="DM"))  # more than 14 days apart: next term's HW 3
    assert drop_done(kept, done) == kept


def test_completed_never_hides_hseapp_or_own_items():
    item = dl("DM HW 3", D(2026, 10, 15), "hseapp", course="DM")
    own = Deadline("DM HW 3", D(2026, 10, 15), "study", "ha", key="ha-1")
    assert drop_done((item, own), (item,)) == (item, own)


def test_a_letter_or_a_chat_moves_the_date_of_hseapp():
    app = dl("Safety HW 3", D(2026, 11, 16), "hseapp")  # the app keeps the first date
    mail = dl("Safety HW 3", D(2026, 11, 20), "mail", course="Safety")  # the letter carries the new one
    tg = dl("DM HW 3 late", D(2026, 10, 18), "tg", course="DM")
    kept = (dl("Calc HW 3", D(2026, 11, 16), "hseapp", course="Calc"),  # another course
            dl("DM HW 3", D(2026, 11, 2), "hseapp", course="DM"),  # more than 14 days apart
            dl("Safety HW 3", D(2026, 11, 20), "ical"))  # RUZ does not move it
    assert drop_moved((app, mail, *kept)) == (mail, *kept)
    assert drop_moved((dl("DM HW 3", D(2026, 10, 15), "hseapp", course="DM"), tg)) == (tg,)
    own = Deadline("Safety HW 3", D(2026, 11, 20), "study", "ha", key="ha-1")
    assert drop_moved((app, own)) == (app, own)  # the owner's own is no letter


def test_merge_still_needs_the_same_date():
    a, b = dl("Calc KR-1", D(2026, 10, 22), "hseapp"), dl("Calc KR-1", D(2026, 10, 23), "tg")
    assert merge_deadlines((a,), (b,)) == (a, b)
    one_word = dl("Midterm", D(2026, 10, 22), "ical")
    assert merge_deadlines((one_word,), (dl("Exam", D(2026, 10, 22)),))[1].title == "Exam"


def test_merge_keeps_the_more_reliable_and_one_group_whole():
    hse = dl("DM HW 3", D(2026, 10, 22), "hseapp", course="DM")
    tg = dl("dm  hw 3", D(2026, 10, 22), "tg")  # the title alone: case and spaces do not matter
    late = dl("DM HW 3 late", D(2026, 10, 22), "tg", course="DM")  # the work inside the other one, by words
    twins = (dl("Calc HW 5", D(2026, 10, 22), "ical"), dl("Calc HW 5", D(2026, 10, 22), "ical"))
    assert merge_deadlines((hse,), (tg,), (late,), twins) == (hse, *twins)  # inside a group: not compared


def test_hidden_also_from_another_source_but_never_own():
    hidden = (dl("Calc HW 3", D(2026, 10, 21), "mail", course="Calc"),)
    tg = dl("Calc HW 3", D(2026, 10, 21), "tg", course="Calc")
    other_day = dl("Calc HW 3", D(2026, 10, 22), "tg", course="Calc")
    own = Deadline("Calc HW 3", D(2026, 10, 21), "study", "ha", key="ha-2")
    assert drop_hidden((tg, other_day, own), hidden) == (other_day, own)


def test_mail_and_telegram_overdue_for_one_day():
    today = D(2026, 10, 18)
    shown = visible((
        dl("Mail yesterday", D(2026, 10, 17), "mail"),
        dl("Mail 2 days ago", D(2026, 10, 16), "mail"),
        dl("TG 2 days ago", D(2026, 10, 16), "tg"),
        dl("HSE 3 days ago", D(2026, 10, 15), "hseapp"),
        dl("HSE 4 days ago", D(2026, 10, 14), "hseapp"),
        dl("Later", D(2026, 10, 25), "tg"),
        dl("Today", today, "ical"),
    ), today)
    assert [d.title for d in shown] == ["Today", "Later", "Mail yesterday", "HSE 3 days ago"]


def test_uid():
    item = dl("DM  HW 5", D(2026, 10, 22), "hseapp")
    expected = "hseapp-" + hashlib.sha1(b"hseapp|dm hw 5|2026-10-22").hexdigest()[:10]
    assert uid(item) == expected == uid(dl("dm hw 5", D(2026, 10, 22), "hseapp"))
    assert uid(Deadline("Dentist", D(2026, 10, 25), "personal", "ha", key="ha-7")) == "ha-7"


# --- the list over our tables ------------------------------------------------------------------

def add(merged, label, due, source="tg", course=None, msg_id=1) -> int:
    with merged.db:
        return merged.deadlines.add({"source": source, "chat_id": None if source == "ical" else CHAT,
                                     "msg_ids": [] if source == "ical" else [msg_id], "course": course,
                                     "label": label, "due": due}, merged.now())


def hseapp(merged, items=(), done=()):
    def rows(raws):
        return [{"id": raw_id, "label": label, "due": due, "course": course, "title": f"Русское {label}"}
                for raw_id, label, due, course in raws]
    merged.store_lists = {"fetched_at": "2026-10-20T12:00:00", "items": rows(items), "done": rows(done)}
    merged.hseapp.store.put_json("hseapp.deadlines", merged.store_lists)


@pytest.fixture
def named(merged):
    subjects = merged.subjects
    subjects.set_manual("Дискретная математика", "Discrete Math", "DM")
    subjects.set_manual("Безопасность жизнедеятельности", "Life Safety", "Safety")
    subjects.set_manual("Основы российской государственности", "Rus Statehood", "RSF")
    return merged


def test_sources_in_their_order(named):
    merged = named
    hseapp(merged, items=[("h1", "DM HW 3", "2026-10-22", "Дискретная математика"),
                          ("h2", "Записаться на майнор", "2026-10-30", None)],
           done=[("h0", "Safety HW 2", "2026-10-13", "Безопасность жизнедеятельности")])
    tg_dm = add(merged, "DM HW 3", "2026-10-22", course="Дискретная математика")  # the same as HSE App X's
    tg_calc = add(merged, "Calc KR-1", "2026-10-23", course="матан")  # a course the dictionary does not know
    mail_calc = add(merged, "Calc KR-1", "2026-10-23", "mail", course="Математический анализ")
    add(merged, "Calc KR-1", "2026-10-23", course="Математический анализ", msg_id=2)  # a second post of it
    add(merged, "Safety HW 2", "2026-10-15", "mail")  # done in HSE App X, the date extended
    midterm = add(merged, "RSF Midterm", "2026-10-26", "ical", course="Основы российской государственности")
    add(merged, "Old HW", "2026-10-16")  # past the three days the table is read for
    moved = add(merged, "Alg HW 1", "2026-10-21")
    newer = add(merged, "Alg HW 1", "2026-10-24")
    gone = add(merged, "Hist essay", "2026-10-25")
    with merged.db:
        merged.deadlines.move(moved, newer)
        merged.deadlines.drop([gone], merged.now())
    own = merged.add("Dentist", D(2026, 10, 21), "personal")

    final = merged.all()
    assert [(d.title, d.source, d.due.isoformat(), d.course, d.ref) for d in final] == [
        ("DM HW 3", "hseapp", "2026-10-22", "DM", "hseapp:h1"),
        ("Записаться на майнор", "hseapp", "2026-10-30", None, "hseapp:h2"),
        ("RSF Midterm", "ical", "2026-10-26", "RSF", f"dl:{midterm}"),
        ("Calc KR-1", "mail", "2026-10-23", None, f"dl:{mail_calc}"),  # mail first, though the chat's came first
        ("Alg HW 1", "tg", "2026-10-24", None, f"dl:{newer}"),
        ("Dentist", "ha", "2026-10-21", None, own.key),
    ]
    assert final[0].name == "Дискретная математика" and final[3].name == "Математический анализ"
    assert tg_dm and tg_calc  # merged away: the more reliable copies stay
    assert [d.title for d in merged.visible()] == ["Dentist", "DM HW 3", "Calc KR-1", "Alg HW 1", "RSF Midterm",
                                                   "Записаться на майнор"]
    assert merged.items()[0] == {"uid": "ha-1", "title": "Dentist", "due": "2026-10-21", "category": "personal",
                                 "source": "ha", "overdue": False}
    assert merged.items()[1]["uid"] == uid(final[0]) and merged.find(uid(final[0])) == final[0]
    assert merged.find("tg-0000000000") is None


def test_the_letters_date_stands_for_hseapps(named):
    hseapp(named, items=[("h3", "Safety HW 3", "2026-11-16", None)])
    letter = add(named, "Safety HW 3", "2026-11-20", "mail", course="Безопасность жизнедеятельности")
    assert [(d.source, d.due.isoformat(), d.ref) for d in named.all()] == [("mail", "2026-11-20", f"dl:{letter}")]


def test_course_without_the_dictionary_is_the_first_word(merged):
    """A course the dictionary does not know yet is compared by the label's first word, like Tablo's None."""
    hseapp(merged, done=[("h0", "Calc HW 2", "2026-10-13", "Математический анализ")])
    add(merged, "Calc HW 2", "2026-10-21", course="матанализ")  # within 14 days of the completed one
    assert merged.all() == ()


def test_overdue_rows_leave_the_list(merged):
    add(merged, "TG yesterday", "2026-10-19")
    add(merged, "TG two days ago", "2026-10-18")
    add(merged, "Midterm 3 days ago", "2026-10-17", "ical")
    hseapp(merged, items=[("h1", "HSE 3 days ago", "2026-10-17", None), ("h2", "HSE 4 days ago", "2026-10-16", None)])
    assert [(d["title"], d["overdue"]) for d in merged.items()] == [
        ("TG yesterday", True), ("HSE 3 days ago", True), ("Midterm 3 days ago", True)]


# --- the owner's changes (HA's to-do lists, the Mini App, the forum) ---------------------------

def test_own_and_hidden(merged):
    calc = add(merged, "Calc HW 3", "2026-10-21", course="Математический анализ")
    prog = add(merged, "Prog HW 1", "2026-10-18")
    own = merged.add("Dentist", D(2026, 10, 25), "personal")
    assert own.key == "ha-1" and merged.changes == [{"ha-1"}]  # it came by the owner: quiet in the forum
    with pytest.raises(ValueError, match="title is empty"):
        merged.add("  ", D(2026, 10, 25), "personal")
    with pytest.raises(ValueError, match="category"):
        merged.add("Gym", D(2026, 10, 25), "sport")
    assert len(merged.todo.own()) == 1

    # Done hides it for good, also when it comes back from another chat.
    [item] = [d for d in merged.all() if d.ref == f"dl:{calc}"]
    merged.remove(item)
    assert merged.changes[-1] == set() and merged.is_hidden(item)
    assert merged.todo.hidden_rows() == [{"uid": uid(item), "source": "tg", "course": None, "title": "Calc HW 3",
                                          "due": "2026-10-21", "ref": f"dl:{calc}"}]
    add(merged, "Calc HW 3", "2026-10-21", course="Математический анализ", msg_id=9)
    assert [d.title for d in merged.all()] == ["Prog HW 1", "Dentist"]

    # Editing one from a source: hidden, an own copy with the new date.
    [old] = [d for d in merged.all() if d.ref == f"dl:{prog}"]
    copy = merged.edit(old, None, D(2026, 10, 29))
    assert (copy.source, copy.key, copy.title) == ("ha", "ha-2", "Prog HW 1") and merged.changes[-1] == {"ha-2"}
    edited = merged.edit(own, "Dentist 10:00", None)
    assert edited.key == own.key and edited.due == own.due and merged.changes[-1] == set()  # a rename only
    assert merged.edit(edited, None, None, "study").category == "study"
    edited = merged.edit(edited, None, None, "personal")
    assert edited.category == "personal" and merged.todo.own()[0].category == "personal"
    assert [(d.title, d.source, d.due.isoformat()) for d in merged.all()] == [
        ("Dentist 10:00", "ha", "2026-10-25"), ("Prog HW 1", "ha", "2026-10-29")]

    # «Вернуть»: by the hidden row (the Mini App) or by the deadline (the forum's button).
    assert merged.unhide(uid(item)) and not merged.unhide(uid(item))
    assert any(d.ref == f"dl:{calc}" for d in merged.all())
    merged.remove(item)
    assert merged.restore(item) and not merged.restore(item) and not merged.is_hidden(item)

    merged.remove(copy)
    merged.remove(edited)
    assert merged.todo.own() == ()


def test_a_renamed_one_stays_hidden(merged):
    """Hidden while HSE App X had no label or the dictionary no abbr: the label that comes later is the same row."""
    hseapp(merged, items=[("h1", "ДЗ 3 по ДМ", "2026-10-22", "Дискретная математика")])
    midterm = add(merged, "Foundations Midterm", "2026-10-26", "ical", course="Foundations of Russian Statehood")
    for d in merged.all():
        merged.remove(d)
    hseapp(merged, items=[("h1", "DM HW 3", "2026-10-22", "Дискретная математика")])
    with merged.db:
        merged.deadlines.rename(midterm, "RSF Midterm", "Foundations of Russian Statehood")
    assert merged.all() == ()
    [renamed] = [d for d in merged.unhidden() if d.source == "hseapp"]
    assert merged.is_hidden(renamed) and merged.restore(renamed) and [d.title for d in merged.all()] == ["DM HW 3"]


def test_rows_long_past_are_pruned(merged):
    old = add(merged, "Old HW", "2026-10-21")
    [item] = merged.all()
    merged.remove(item)
    merged.add("Old own", D(2026, 10, 21), "study")
    merged.now.moment += dt.timedelta(days=31)  # Nov 20: 30 days past is not yet more than 30
    merged.add("New own", D(2026, 11, 25), "study")
    assert len(merged.todo.hidden_rows()) == 1 and len(merged.todo.own()) == 2
    merged.now.moment += dt.timedelta(days=1)
    merged.add("Newer own", D(2026, 11, 26), "study")  # every change cleans up
    assert merged.todo.hidden_rows() == [] and [d.title for d in merged.todo.own()] == ["New own", "Newer own"]
    assert old


def test_prune_alone(merged):
    merged.add("Old own", D(2026, 10, 21), "study")
    merged.now.moment += dt.timedelta(days=32)
    merged.prune()
    assert merged.todo.own() == ()


def test_a_failed_change_saves_nothing(merged, monkeypatch):
    def broken(*args):
        merged.db.execute("insert into own_deadlines(title, due, category) values('x', '2026-10-21', 'study')")
        raise ValueError("broken")

    monkeypatch.setattr(merged.todo, "prune", broken)
    with pytest.raises(ValueError):
        merged.add("Gym", D(2026, 10, 25), "personal")
    assert merged.todo.own() == () and merged.changes == []


def test_by_ref_follows_moves_and_finds_hidden_ones(merged):
    first = add(merged, "DM HW 3", "2026-10-21")
    second = add(merged, "DM HW 3", "2026-10-23")
    with merged.db:
        merged.deadlines.move(first, second)
    found = merged.by_ref(f"dl:{first}")
    assert found.ref == f"dl:{second}" and found.due == D(2026, 10, 23)
    merged.remove(found)
    assert merged.by_ref(f"dl:{first}") == found and merged.all() == ()
    with merged.db:
        merged.deadlines.drop([second], merged.now())
    assert merged.by_ref(f"dl:{first}") is None and merged.by_ref("hseapp:nope") is None


def test_by_ref_finds_the_copy_that_stands_for_it(merged):
    tg = add(merged, "DM HW 3", "2026-10-22")
    hseapp(merged, items=[("h1", "DM HW 3", "2026-10-22", None)])
    assert merged.by_ref(f"dl:{tg}").ref == "hseapp:h1"
    hseapp(merged, done=[("h1", "DM HW 3", "2026-10-22", None)])  # done there: nothing stands for it
    assert merged.by_ref(f"dl:{tg}") is None


def test_latest_survives_a_loop(merged):
    a, b = add(merged, "A", "2026-10-21"), add(merged, "A", "2026-10-22")
    with merged.db:
        merged.db.execute("update deadlines set moved_by = ? where id = ?", (b, a))
        merged.db.execute("update deadlines set moved_by = ? where id = ?", (a, b))
    assert merged.deadlines.latest(a)["id"] in (a, b) and merged.deadlines.latest(999) is None


def test_constants_are_tablos():
    assert (merge.DONE_WINDOW_DAYS, merge.OVERDUE_DAYS, merge.OVERDUE_DAYS_BY_SOURCE) == (14, 3, {"mail": 1, "tg": 1})
