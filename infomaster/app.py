"""Wiring: one asyncio process with the bot and two HTTP ports (§3, §8.1)."""

import asyncio
import contextlib
import logging
import signal
from pathlib import Path

from aiohttp import web

from . import clock
from .collectors.files import Files
from .collectors.grades import GradeSheets
from .collectors.hseapp import CALLBACK_PATH, HseApp
from .collectors.inbox import Inbox
from .collectors.mail import Mail
from .collectors.ruz import Diff, Ruz
from .collectors.telegram import Twin
from .config import Env
from .i18n import Phrase
from .llm.budget import Budget
from .llm.client import Haiku
from .llm.journal import Journal
from .outputs import api, miniapp
from .outputs.deadlines import BUTTON as DEADLINE_BUTTON
from .outputs.deadlines import DeadlineTopic
from .outputs.feed import BUTTON as NEWS_BUTTON
from .outputs.feed import Feed
from .outputs.forum import Forum
from .outputs.grades import GradeTopic
from .outputs.ical_feed import CalendarFeed
from .outputs.sheets import SheetTopic
from .outputs.summaries import BUTTON as SUMMARY_BUTTON
from .outputs.summaries import COMMAND as SUMMARY_COMMAND
from .outputs.summaries import SummaryTopic
from .outputs.timetable import BUTTON, Timetable
from .processing.batches import Batches, Changes
from .processing.deadlines import Deadlines
from .processing.events import Events
from .processing.grades import Grades
from .processing.hseapp import HseAppDeadlines
from .processing.images import Images
from .processing.merge import DeadlineList
from .processing.news import News
from .processing.schedule import MidtermChanges, Schedule
from .processing.sheets import Sheets
from .processing.subjects import Subjects
from .processing.summaries import Summaries
from .settings import ChatSettings, Settings
from .store import Store

log = logging.getLogger(__name__)

PRUNE_EVERY = 6 * 3600  # seconds between removals of raw messages past their 14 days


def twin_session(data_dir: Path) -> Path:
    return data_dir / "twin"


class Service:
    def __init__(self, env: Env, data_dir: Path):
        data_dir.mkdir(parents=True, exist_ok=True)
        data_dir.chmod(0o700)  # sessions and tokens: the owner only (§10)
        self.env = env
        self.store = Store(data_dir / "infomaster.db")
        self.settings = Settings(self.store)
        self.journal = Journal(self.store)
        self.budget = Budget(self.settings, self.journal, self.store)
        self.llm = Haiku(self.budget, self.journal)
        self.forum = Forum(env, self.store, self.budget, session=data_dir / "bot")
        self.budget.notify = self.forum.on_budget
        self.inbox = Inbox(self.store)
        self.chat_settings = ChatSettings(self.store)
        self.twin = Twin(env, self.inbox, self.chat_settings, session=twin_session(data_dir),
                         notify=self._to_system)
        self.twin.notify_login = self._twin_notice
        self.news, self.deadlines = News(self.store), Deadlines(self.store)
        self.subjects = Subjects(self.store, self.llm)
        self.ruz = Ruz(env.ruz_ical_url, self.store)
        self.schedule = Schedule(self.store, self.ruz, Events(self.store), self.deadlines, self.subjects, self.inbox)
        self.batches = Batches(self.store, self.settings, self.chat_settings, self.inbox, self.news, self.deadlines,
                               self.schedule, self.llm)
        # PDFs come through the twin when a batch needs them; the sheets among them go to «Листочки» (§6.3).
        self.files = Files(self.store, data_dir)
        self.files.download = self.twin.download
        self.sheets = Sheets(self.store, self.settings, self.chat_settings, self.inbox, self.files, self.deadlines,
                             self.subjects, self.llm)
        self.sheet_topic = SheetTopic(self.forum, self.store, self.inbox, self.sheets)
        self.batches.files, self.batches.sheets = self.files, self.sheets
        # Images of chats with «Картинки» come the same way and are read by Haiku within their daily number (D12).
        self.images = Images(self.settings, self.budget, self.files, self.llm)
        self.batches.images = self.images
        self.feed = Feed(self.forum, self.store, self.inbox, self.news, self.settings, env.owner_id)
        self.timetable = Timetable(self.forum, self.store, self.inbox, self.schedule, env.owner_id)
        self.calendar = CalendarFeed(self.schedule)
        self.hseapp = HseApp(self.store, self.settings, callback_url=env.public_url + CALLBACK_PATH)
        self.hseapp_deadlines = HseAppDeadlines(self.store, self.subjects, self.llm)
        self.mail = Mail(env, self.store, self.settings, self.inbox)
        # The one list of deadlines (§5.5): the API for Tablo and HA, the «Дедлайны» topic and the Mini App read it.
        self.merged = DeadlineList(self.store, self.deadlines, self.hseapp_deadlines, self.subjects)
        self.deadline_topic = DeadlineTopic(self.forum, self.store, self.inbox, self.merged, env.owner_id)
        # Summaries are made from the raw messages, the daily one and /summary (§6.6).
        summaries = Summaries(self.chat_settings, self.inbox, self.llm)
        summaries.files = self.files  # what the batches read on images
        self.summary_topic = SummaryTopic(self.forum, self.store, self.inbox, self.chat_settings, summaries,
                                          self.settings, env.owner_id, env.forum_id)
        # Grades from teachers' Google Sheets, new ones to «Оценки» (§6.7).
        self.grade_sheets = GradeSheets(self.settings)
        self.grades = Grades(self.store, self.settings, self.llm)
        self.grade_topic = GradeTopic(self.forum, self.store, self.grades)
        # GET /api/status and the Mini App's screens read these by name (the «Расписание» screen reads "ruz", the
        # «HSE App X» one "hseapp", the «Чаты» one "images").
        self.sources = {"telegram": self.twin.status, "batches": self.batches.status, "ruz": self._ruz_status,
                        "hseapp": self._hseapp_status, "mail": self.mail.status, "sheets": self.sheets.status,
                        "summaries": self.summary_topic.status, "grades": self._grades_status,
                        "images": self.images.status}
        self.inbox.wake = self.batches.wake
        self.batches.publish = self._publish
        self.batches.notify = self._to_system
        self.batches.mail = self.mail.configured
        self.mail.notify = self._to_system
        self.ruz.on_poll = self._after_poll
        self.ruz.notify = self._to_system
        self.schedule.on_midterms = self._publish_midterms
        self.hseapp.on_poll = self.hseapp_deadlines.after_poll
        self.hseapp.notify = self._hseapp_notice
        self.hseapp_deadlines.on_change = self._hseapp_changed
        self.merged.on_change = self.deadline_topic.changed
        self.grade_sheets.on_sheet = self.grades.apply
        self.grade_sheets.on_poll = self.grade_topic.publish
        self.grade_sheets.notify = self._to_system
        self.grades.wake = self.grade_sheets.wake
        self.subjects.on_manual = self._dictionary_changed
        self.forum.on_button(BUTTON, self.timetable.on_button)
        self.forum.on_button(DEADLINE_BUTTON, self.deadline_topic.on_button)
        self.forum.on_button(NEWS_BUTTON, self.feed.on_button)
        self.forum.on_button(SUMMARY_BUTTON, self.summary_topic.on_button)
        self.forum.on_command(SUMMARY_COMMAND, self.summary_topic.on_command)

    def _to_system(self, text: Phrase) -> None:
        self.forum.spawn(self.forum.post("system", text))

    def _twin_notice(self, text: Phrase) -> None:
        """Twin.notify_login: the line gets the button that leads to the login on the Mini App's «Chats» screen."""
        button = self.forum.app_button(self.forum.tr("Log the twin in", "Войти в твинк"), "chats")
        self.forum.spawn(self.forum.post("system", text, buttons=[button] if button else None))

    def _hseapp_notice(self, text: Phrase, login: bool) -> None:
        """HseApp.notify: a line for the System topic; one that asks for a login gets the button to the Mini App."""
        button = self.forum.app_button(self.forum.tr("Log in to HSE App X", "Войти в HSE App X"), "hseapp") \
            if login else None
        self.forum.spawn(self.forum.post("system", text, buttons=[button] if button else None))

    async def _publish(self, changes: Changes) -> None:
        """Batches.publish: the news, the timetable, the sheets waiting (this run's and any a failure left), then
        the deadlines (the merged list, with what the run gave, the sheets read and the Midterms it moved); one
        failing does not stop the others."""
        for publish in (self.feed.publish, self.timetable.publish):
            try:
                await publish(changes)
            except Exception:
                log.exception("publishing the batch results failed")
        try:
            await self.sheet_topic.publish()
        except Exception:
            log.exception("posting the sheets failed")
        await self._refresh_deadlines()

    async def _after_poll(self, diff: Diff) -> None:
        """Ruz.on_poll: the processing of the timetable, then what changed goes to the forum. The Midterms of a first
        poll (the first one ever, or after the link changed) join the deadlines quietly, like the classes."""
        midterms = await self.schedule.after_poll(diff)
        if diff.first:  # before any await: no refresh may see them first
            self.deadline_topic.quiet(f"dl:{deadline_id}" for deadline_id in
                                      [*midterms.deadlines, *(new for _, new in midterms.moved), *midterms.gone])
        try:
            await self.timetable.publish_ruz(diff)
        except Exception:
            log.exception("posting the RUZ changes failed")
        await self._refresh_deadlines()  # renamed Midterms too: they only edit the list

    async def _publish_midterms(self, midterms: MidtermChanges) -> None:
        """Schedule.on_midterms: the owner's hiding of a correction moved a Midterm. The news list is the batches' to
        rebuild, so this task never races them over it."""
        await self._refresh_deadlines()

    async def _hseapp_changed(self, first: bool) -> None:
        """HseAppDeadlines.on_change: the lists of HSE App X changed. The first poll joins the deadlines quietly."""
        if first:  # before any await: no refresh may see them first
            self.deadline_topic.quiet(f"hseapp:{item['id']}" for item in self.hseapp_deadlines.current())
        await self._refresh_deadlines()

    def _dictionary_changed(self) -> None:
        """Subjects.on_manual: the Midterms take the new abbr now, not at the next RUZ poll; the list follows."""
        self.schedule.sync_midterms()
        self.forum.spawn(self._refresh_deadlines())

    async def _refresh_deadlines(self) -> None:
        try:
            await self.deadline_topic.refresh()
        except Exception:
            log.exception("refreshing the deadlines failed")

    async def run(self) -> None:
        stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, stop.set)

        await self.forum.start()
        runners, tasks = [], []
        try:
            for app, port in ((miniapp.create_app(self.env, self.budget, self.journal, self.inbox, self.chat_settings,
                                                  self.sources, self.subjects, self.schedule, self.hseapp,
                                                  self.merged, self.settings, self.sheet_topic,
                                                  self.summary_topic, self.grades, self.twin, self.calendar),
                               self.env.public_port),
                              (api.create_app(self.env, self.budget, self.sources, self.calendar, self.merged,
                                              self.grades, self.news, self.settings),
                               self.env.internal_port)):
                runner = web.AppRunner(app, access_log=None)
                await runner.setup()
                await web.TCPSite(runner, "0.0.0.0", port).start()
                runners.append(runner)
            log.info("public port %d (Mini App), internal port %d (API)", self.env.public_port,
                     self.env.internal_port)
            if not self.env.calendar_secret:
                log.info("CALENDAR_SECRET is not set: the iCal feeds for Tablo are off")
            if not self.env.calendar_public_secret:
                log.info("CALENDAR_PUBLIC_SECRET is not set: no iCal feeds behind Funnel")
            await self.forum.post("system", Phrase("infomaster is up.", "infomaster запущен."))
            await self.twin.start()  # without a login the service runs on, and the twin says so
            tasks.append(asyncio.create_task(self._prune()))
            tasks.append(asyncio.create_task(self.batches.run()))
            tasks.append(asyncio.create_task(self.ruz.run()))
            tasks.append(asyncio.create_task(self.hseapp.run()))
            tasks.append(asyncio.create_task(self.mail.run()))
            tasks.append(asyncio.create_task(self.deadline_topic.run()))
            tasks.append(asyncio.create_task(self.summary_topic.run()))
            tasks.append(asyncio.create_task(self.grade_sheets.run()))

            await self._watch(stop)
        finally:
            for task in tasks:
                task.cancel()
            for runner in runners:
                await runner.cleanup()
            for client in (self.twin, self.forum):
                with contextlib.suppress(Exception):
                    await client.stop()
            self.store.close()

    async def _watch(self, stop: asyncio.Event) -> None:
        """Until a signal, or until the bot or the twin loses Telegram for good (Docker restarts the service then).
        A login or a logout in the Mini App changes the twin's client: the new one is watched, a logout is no loss."""
        stopped = asyncio.ensure_future(stop.wait())
        try:
            while not stopped.done():
                self.twin.changed.clear()
                # Telethon's own futures: waited on, never cancelled.
                watched = {self.forum.client.disconnected: "the bot"}
                if self.twin.client is not None:
                    watched[self.twin.client.disconnected] = "the twin"
                changed = asyncio.ensure_future(self.twin.changed.wait())
                await asyncio.wait({*watched, stopped, changed}, return_when=asyncio.FIRST_COMPLETED)
                changed.cancel()
                if self.twin.changed.is_set():
                    continue
                lost = [who for future, who in watched.items() if future.done()]
                if lost:
                    log.error("%s lost Telegram for good, exiting so Docker restarts the service", " and ".join(lost))
                    return
        finally:
            stopped.cancel()

    def _ruz_status(self) -> dict:
        """RUZ and the naming of its subjects, which waits for Haiku (§4.2)."""
        return {**self.ruz.status(), "subjects_error": self.subjects.error}

    def _grades_status(self) -> dict:
        """The sheets (by id: never the links or the name) and the new grades in the window (§8.2)."""
        return {**self.grade_sheets.status(), **self.grades.status()}

    def _hseapp_status(self) -> dict:
        """HSE App X: the login and the polls, the lists kept and their English labels (§5.1, §8.2)."""
        return {**self.hseapp.status(), **self.hseapp_deadlines.status()}

    async def _prune(self) -> None:
        while True:
            try:
                if removed := self.inbox.prune(clock.now_utc()):
                    log.info("removed %d raw messages older than 14 days", removed)
                if removed := self.files.prune(clock.now_utc()):
                    log.info("removed %d downloaded files older than 14 days", removed)
            except Exception:
                log.exception("pruning raw messages failed")
            await asyncio.sleep(PRUNE_EVERY)
