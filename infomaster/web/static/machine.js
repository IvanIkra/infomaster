// «Machine» (§8.6, stage 16.6, D65): the sources and their freshness, the journal with undo, and the settings moved
// over from the Mini App. A change of a setting is journaled on the server; the note at the top offers its undo.
import { useEffect, useState } from "preact/hooks";
import { ApiError, CHANGED, ICONS, api, changed, go, html, icon, toast } from "./common.js";
import { getLang, setLang } from "./i18n.js";

const TEXTS = {
  machine: ["Machine", "Машина"],
  back: ["Back", "Назад"],
  attention: ["Needs attention", "Требует внимания"],
  allWorks: ["Everything works.", "Всё работает."],
  sources: ["Sources", "Источники"],
  setup: ["Setup", "Настройка"],
  service: ["The service", "Сервис"],
  twin: ["Twin and chats", "Твинк и чаты"],
  mail: ["Mail", "Почта"],
  ruz: ["RUZ timetable", "Расписание РУЗ"],
  hseapp: ["HSE App X", "HSE App X"],
  grades: ["Gradebooks", "Таблицы оценок"],
  wiki: ["Course wiki", "Вики курсов"],
  batches: ["Batches", "Пачки"],
  images: ["Images", "Картинки"],
  llm: ["LLM spending", "Расход LLM"],
  ok: ["works", "работает"],
  late: ["late", "запаздывает"],
  error: ["failing", "ошибка"],
  login: ["login needed", "нужен вход"],
  off: ["off", "выкл."],
  idle: ["waiting", "ждёт"],
  read: ["read", "прочитано"],
  countTwin: ["{n} chats on", "чатов включено: {n}"],
  countMail: ["{n} letters", "писем: {n}"],
  countRuz: ["{n} classes", "пар: {n}"],
  countHseapp: ["{n} deadlines", "сроков: {n}"],
  countGrades: ["{n} gradebooks", "таблиц: {n}"],
  countWiki: ["{n} pages", "страниц: {n}"],
  countBatches: ["{n} in the queue", "в очереди: {n}"],
  countImages: ["{n} today", "сегодня: {n}"],
  journal: ["Journal of changes", "Журнал изменений"],
  journalHint: ["Your changes here and Claude's through the connector. Undo puts a change back while what it touched is unchanged.",
                "Ваши изменения здесь и изменения Claude через коннектор. «Отменить» возвращает изменение, пока затронутое не поменялось."],
  noChanges: ["No changes yet.", "Изменений пока нет."],
  viaWeb: ["you", "вы"],
  viaClaude: ["Claude", "Claude"],
  undo: ["Undo", "Отменить"],
  undone: ["undone", "отменено"],
  undoneNote: ["Undone", "Отменено"],
  cannotUndo: ["cannot be undone", "не отменить"],
  saved: ["Saved", "Сохранено"],
  processing: ["Batches and news", "Пачки и новости"],
  courses: ["Course dictionary", "Словарь курсов"],
  connector: ["Claude connector", "Коннектор Claude"],
  notifications: ["Notifications", "Уведомления"],
  loginDevices: ["Login and devices", "Вход и устройства"],
  language: ["Language", "Язык"],
  secrets: ["The secrets (tokens, the mail password, the RUZ link) are in .env on the server.",
            "Секреты (токены, пароль почты, ссылка РУЗ) — в .env на сервере."],
  // the twin and the chats
  twinTitle: ["Twin", "Твинк"],
  twinReads: ["reads the enabled chats", "читает включённые чаты"],
  logOut: ["Log out", "Выйти"],
  logOutTwin: ["Log out of the twin's account? The chats stop being read, their settings stay.",
               "Выйти из аккаунта твинка? Чаты перестанут читаться, их настройки останутся."],
  twinLogin: ["Twin login", "Вход твинка"],
  phone: ["Phone", "Телефон"],
  code: ["Code", "Код"],
  password: ["Password", "Пароль"],
  getCode: ["Get a code", "Получить код"],
  logIn: ["Log in", "Войти"],
  cancel: ["Cancel", "Отмена"],
  phoneHint: ["A Telegram account of its own, not your main one and not a bot.",
              "Отдельный аккаунт Telegram, не ваш основной и не бот."],
  codeHint: ["The code comes to the twin's Telegram ({phone}) or by SMS. Do not forward it in Telegram: then it stops working.",
             "Код приходит в Telegram твинка ({phone}) или по SMS. Не пересылайте его в Telegram: тогда он перестаёт действовать."],
  passwordHint: ["The twin's cloud password.", "Облачный пароль твинка."],
  findChat: ["Find a chat", "Найти чат"],
  chatsOn: ["Read", "Читаются"],
  chatsOff: ["Off", "Выключены"],
  chatsNone: ["No chats yet: they come when the twin logs in to Telegram.",
              "Чатов пока нет: они появятся, когда твинк войдёт в Telegram."],
  nothingFound: ["Nothing found.", "Ничего не нашлось."],
  study: ["study", "учебный"],
  importantOnly: ["important only", "только важное"],
  readChat: ["Read the chat", "Читать чат"],
  whatToDo: ["What to do with it", "Что с ним делать"],
  flagStudy: ["Study", "Учебный"],
  flagStudyHint: ["news rank, deadlines", "ранг новостей, дедлайны"],
  flagUrgent: ["Urgent", "Срочно"],
  flagUrgentHint: ["a pin, a PDF or a message of an important person: parsed at once",
                   "закреп, PDF или сообщение важного человека — разбор сразу"],
  flagImages: ["Images", "Картинки"],
  flagImagesHint: ["Haiku reads photos and screenshots, within the images a day",
                   "фото и скриншоты читает Haiku, в пределах числа картинок в день"],
  flagPdf: ["Whole PDF", "PDF целиком"],
  flagPdfHint: ["sheets: a summary, deadlines from every page, what changed",
                "листочки: краткое содержание, дедлайны со всех страниц, что изменилось"],
  parsing: ["Parsing", "Разбор"],
  messages: ["Messages", "Сообщения"],
  allMessages: ["all", "все"],
  howOften: ["How often", "Как часто"],
  asAll: ["as all chats", "как у всех"],
  everyH: ["every {n} h", "каждые {n} ч"],
  importantHint: ["Important: the people below, pins, files, dates and the replies to them.",
                  "Важное — люди ниже, закрепы, файлы, даты и ответы на них."],
  noteForModel: ["Note for the model", "Заметка для модели"],
  noteHint: ["The batches see it: the chat's course, who is who.", "Это видит разбор пачек: предмет чата, кто есть кто."],
  people: ["Important people", "Важные люди"],
  peopleHint: ["For «important only» and «Urgent»: the group head, the teachers.",
               "Для «только важное» и «Срочно»: староста, преподаватели."],
  nobody: ["Nobody has written yet.", "Пока никто не писал."],
  messagesN: ["messages: {n}", "сообщений: {n}"],
  notWritten: ["has not written yet", "ещё не писал"],
  // HSE App X
  hseIntro: ["Deadlines from the HSE app.", "Дедлайны из приложения ВШЭ."],
  hsePoll: ["Poll HSE App X", "Опрашивать HSE App X"],
  hseOne: ["One client per login: HSE App X must be off in Tablo.",
           "Только один клиент на вход: в Tablo HSE App X должен быть выключен."],
  hseNeedsLogin: ["A login is needed: the deadlines are not updated.", "Нужен вход: дедлайны не обновляются."],
  hseActive: ["Active deadlines", "Активные дедлайны"],
  hseDone: ["Done", "Выполненные"],
  updated: ["Updated", "Обновлено"],
  soon: ["soon", "вот-вот"],
  logInAgain: ["Log in again", "Войти заново"],
  hseOnce: ["Once is enough: after that the service renews the login itself every 15 minutes.",
            "Достаточно одного раза: дальше сервис сам продлевает вход раз в 15 минут."],
  hseOpened: ["The login is open in the browser. Come back here after it: the screen updates by itself.",
              "Вход открыт в браузере. После него вернитесь сюда: экран обновится сам."],
  hseLoggedIn: ["Logged in. The deadlines will be updated within a minute.",
                "Вход выполнен. Дедлайны обновятся в течение минуты."],
  hseNoReturn: ["The browser did not come back", "Браузер не вернулся"],
  hseAppAddress: ["Log in with the app's address", "Войти с адресом приложения"],
  hseSave: ["Save the login", "Сохранить вход"],
  hseFallback: ["After the login the browser comes back to {url} by itself. If that page did not open, copy its whole address (it has code= in it) and paste it here within a minute. If HSE does not let you in, log in with the app's address: the browser cannot open ru.hse.hseappx://…, and that address is the one to copy (Safari loses it, Chrome is needed).",
                "После входа браузер сам возвращается на {url}. Если эта страница не открылась, скопируйте её адрес целиком (в нём есть code=) и вставьте сюда в течение минуты. Если ВШЭ не пускает, войдите с адресом приложения: браузер не сможет открыть ru.hse.hseappx://… — этот адрес и нужно скопировать (Safari его теряет, нужен Chrome)."],
  lastError: ["The last error: ", "Последняя ошибка: "],
  // processing
  batchEvery: ["Batches", "Пачки"],
  batchHint: ["How often the chats' messages are parsed; a chat can have its own.",
              "Как часто разбираются сообщения чатов; у чата может быть своё."],
  newsLives: ["News lives, hours", "Новость живёт, часов"],
  aboutMe: ["About me, for the models", "Обо мне — для моделей"],
  aboutMeHint: ["Group, subgroup, English group, electives: what is meant for others does not count.",
                "Группа, подгруппа, группа английского, майноры: что адресовано другим, не учитывается."],
  wikiPages: ["Course wiki pages", "Страницы вики курсов"],
  wikiHint: ["New sheets and homework of these pages come as letters. One address a line.",
             "Новые листочки и ДЗ с этих страниц приходят письмами. По адресу в строке."],
  reminders: ["Deadline reminders", "Напоминания о сроках"],
  remindersOn: ["Remind", "Напоминать"],
  eve: ["The evening before", "Накануне"],
  morning: ["On the day", "В день срока"],
  wholePdf: ["Whole PDFs", "PDF целиком"],
  pdfPages: ["Pages, at most", "Страниц, не больше"],
  pdfTokens: ["Tokens, at most", "Токенов, не больше"],
  pdfHint: ["For chats with «Whole PDF»: what comes first.", "Для чатов с «PDF целиком»: что наступит раньше."],
  imagesDaily: ["A day, at most", "В день, не больше"],
  imagesHint: ["Haiku reads photos and screenshots in chats with «Images»; above 80 % of the LLM limit it does not.",
               "Фото и скриншоты в чатах с «Картинками» читает Haiku; выше 80 % лимита LLM — не читает."],
  save: ["Save", "Сохранить"],
  // dictionary
  coursesHint: ["How the sources name a course, its short English title and abbreviation. Yours are never overwritten by Haiku.",
                "Как источники называют курс, его короткое английское название и сокращение. Ваши Haiku не перезапишет."],
  yours: ["yours", "ваше"],
  name: ["Name in the sources", "Название в источниках"],
  title: ["Title", "Название"],
  abbr: ["Abbreviation", "Сокращение"],
  addEntry: ["Add an entry", "Добавить запись"],
  removeEntry: ["Delete", "Удалить"],
  // the LLM
  todaySpent: ["Today", "Сегодня"],
  limit: ["Daily limit, $", "Лимит в день, $"],
  raiseToday: ["One more limit until midnight", "Ещё один лимит до полуночи"],
  week: ["Week", "Неделя"],
  month: ["Month", "Месяц"],
  forecast: ["Month forecast", "Прогноз на месяц"],
  byTask: ["By task, this week", "По задачам за неделю"],
  byChat: ["By chat, this week", "По чатам за неделю"],
  days30: ["Last 30 days", "Последние 30 дней"],
  runs: ["calls: {n}", "вызовов: {n}"],
  stopped: ["the limit stopped calls", "лимит остановил вызовы"],
  // grades
  student: ["Your name as the gradebooks have it", "Ваше ФИО, как в таблицах"],
  studentAsk: ["Change the name? The gradebooks are read anew: the first reading tells of no new grades.",
               "Сменить ФИО? Таблицы прочитаются заново: первое чтение — без сообщений о новых оценках."],
  windowDays: ["A grade is new, days", "Оценка новая, дней"],
  addBook: ["Add a gradebook", "Добавить таблицу"],
  bookUrl: ["Google Sheets or Dropbox link", "Ссылка Google Таблиц или Dropbox"],
  bookLabel: ["Course, short", "Курс, коротко"],
  notYet: ["not yet", "ещё нет"],
  readNow: ["Read now", "Прочитать сейчас"],
  reading: ["Reading the gradebook: the result comes in a few seconds.",
            "Таблица читается: результат будет через несколько секунд."],
  open: ["Open", "Открыть"],
  removeBook: ["Remove the gradebook", "Удалить таблицу"],
  removeBookAsk: ["Remove «{name}»? Its grades are forgotten.", "Удалить «{name}»? Её оценки забудутся."],
  tabs: ["Tabs", "Вкладки"],
  noTabs: ["No tab with your row.", "Нет вкладок с вашей строкой."],
  works: ["works: {n}", "работ: {n}"],
  skipped: ["skipped", "пропущена"],
  notMarked: ["Haiku could not mark it up", "Haiku не смогла разметить"],
  byYou: ["marked up by you", "размечена вами"],
  byHaiku: ["marked up by Haiku", "размечена Haiku"],
  readTab: ["Read this tab", "Читать вкладку"],
  yourRow: ["Your row is row {n}. On the right: the number in it and the grade on 0–10.",
            "Ваша строка — {n}-я. Справа — число в ней и оценка по шкале 0–10."],
  column: ["Column", "Колонка"],
  outOf: ["Out of", "Из"],
  bonus: ["Bonus column", "Колонка бонуса"],
  none: ["none", "нет"],
  addColumn: ["Add a column", "Добавить колонку"],
  removeWork: ["Drop the work", "Убрать работу"],
  allColumns: ["All columns of your row", "Все колонки вашей строки"],
  backToHaiku: ["Give it back to Haiku", "Вернуть разметку Haiku"],
  backToHaikuAsk: ["Drop your markup of this tab? Haiku's applies again.",
                   "Сбросить вашу разметку вкладки? Снова будет разметка Haiku."],
  // the connector
  connectorUrl: ["The address to add in claude.ai: Settings → Connectors → Add custom connector.",
                 "Адрес для claude.ai: Настройки → Коннекторы → Добавить свой коннектор."],
  copy: ["Copy", "Копировать"],
  copied: ["Copied", "Скопировано"],
  connections: ["Connections", "Подключения"],
  noConnections: ["No connections.", "Подключений нет."],
  revoke: ["Revoke", "Отозвать"],
  connected: ["connected", "подключён"],
  used: ["used", "использован"],
  tools: ["Tools: {read} to read, {write} to change.", "Инструментов: {read} для чтения, {write} для изменений."],
  // notifications
  kinds: ["What comes", "Что приходит"],
  kindDeadlines: ["New and moved deadlines", "Новые и перенесённые сроки"],
  kindReminders: ["Reminders", "Напоминания"],
  kindFailures: ["Failures and logins", "Сбои и входы"],
  kindLlm: ["The LLM limit spent", "Исчерпан лимит LLM"],
  quiet: ["Quiet hours", "Тихие часы"],
  quietHint: ["Notifications of these hours come without a sound. Moscow time.",
              "Уведомления в эти часы приходят без звука. Время московское."],
  quietOn: ["Without a sound at night", "Ночью без звука"],
  from: ["From", "С"],
  to: ["To", "До"],
};

export const m = (key, values) => {
  const pair = TEXTS[key];
  let text = pair ? pair[getLang() === "ru" ? 1 : 0] : key;
  for (const [name, value] of Object.entries(values || {})) text = text.replace(`{${name}}`, value);
  return text;
};

// '12:45' today, '20 Nov, 12:45' before, of a Moscow local time without a zone
export function fresh(iso) {
  if (!iso) return "";
  const now = new Date(), date = iso.slice(0, 10);
  const todayIso = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
  if (date === todayIso) return iso.slice(11, 16);
  const shown = new Date(date + "T12:00:00").toLocaleDateString(getLang() === "ru" ? "ru-RU" : "en-GB",
                                                               { day: "numeric", month: "short" });
  return `${shown}, ${iso.slice(11, 16)}`;
}

const usd = (value) => "$" + (Number(value) || 0).toFixed(2);

// A screen's data with its loading and changes: run(job) shows what the job gives and a note with the undo of the
// change it made.
function useMachine(path, onOut) {
  const [data, setData] = useState(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const fail = (e) => { if (e instanceof ApiError && e.status === 401) onOut(); else setError(e.message); };
  const load = async () => {
    try { setData(await api("GET", path)); setError(""); } catch (e) { fail(e); }
  };
  useEffect(() => {
    load();
    window.addEventListener(CHANGED, load);
    return () => window.removeEventListener(CHANGED, load);
  }, [path]);
  const run = async (job, text) => {
    setBusy(true); setError("");
    try {
      const answer = await job();
      if (answer && answer.change) toast(text || m("saved"), answer.change.undo ? answer.change.id : null);
      if (answer) setData((old) => ({ ...old, ...(answer.settings || answer) }));
      return answer;
    } catch (e) { fail(e); return null; } finally { setBusy(false); }
  };
  return { data, setData, error, busy, run, reload: load };
}

function Screen({ title, sub, back = "#/machine", error, children }) {
  return html`<main class="screen">
    <header class="top">
      <a class="rb" href=${back} aria-label=${m("back")}>${icon(ICONS.back)}</a>
      <div><h1>${title}</h1>${sub && html`<div class="mut small">${sub}</div>`}</div>
    </header>
    <div class="err" role="alert">${error}</div>
    <div class="stack mach">${children}</div>
  </main>`;
}

function Section({ title, foot, children }) {
  return html`<section class="stack" style="gap:6px">
    ${title && html`<h2>${title}</h2>`}
    <div class="box list">${children}</div>
    ${foot && html`<div class="note small">${foot}</div>`}
  </section>`;
}

function Switch({ on, busy, onChange, label }) {
  return html`<label class="switch"><input type="checkbox" role="switch" checked=${on} disabled=${busy}
    aria-label=${label} onChange=${(e) => onChange(e.target.checked)} /><span></span></label>`;
}

const Go = ({ href, title, value, bad }) => html`<a class="link" href=${href}>${title}
  <span>${value !== undefined && value !== null && html`<em class=${bad ? "bad" : ""}>${value}</em>`}
  ${icon(ICONS.next, 16)}</span></a>`;

const State = ({ state }) => html`<span class=${"st st-" + state}>${m(state)}</span>`;

// The field that saves on leaving it (change), not on every key.
function Field({ value, onSave, ...rest }) {
  return html`<input class="field" value=${value ?? ""} onChange=${(e) => onSave(e.target.value)} ...${rest} />`;
}

// --- the overview ----------------------------------------------------------------------------------------------

const COUNTS = { twin: "countTwin", mail: "countMail", ruz: "countRuz", hseapp: "countHseapp", grades: "countGrades",
                 wiki: "countWiki", batches: "countBatches", images: "countImages" };
const SCREENS = { twin: "#/machine/chats", hseapp: "#/machine/hseapp", grades: "#/machine/grades",
                  wiki: "#/machine/processing", images: "#/machine/processing", llm: "#/machine/llm",
                  batches: "#/machine/processing" };

function SourceRow({ s, llm }) {
  const facts = [];
  if (s.ok_at) facts.push(`${m("read")} ${fresh(s.ok_at)}`);
  if (s.count !== null && s.count !== undefined && COUNTS[s.key]) facts.push(m(COUNTS[s.key], { n: s.count }));
  if (s.key === "llm" && llm) facts.push(`${usd(llm.spent_usd)} / ${usd(llm.limit_usd)}`);
  const body = html`<div><b>${m(s.key)}</b>
      <div class="mut small">${facts.join(" · ")}</div>
      ${s.error && s.state !== "ok" && html`<div class="err small">${s.error}</div>`}</div>
    <${State} state=${s.state} />`;
  return SCREENS[s.key] ? html`<a class="row" href=${SCREENS[s.key]}>${body}</a>` : html`<div class="row">${body}</div>`;
}

export function Machine({ onOut, onLang }) {
  const { data, error, run } = useMachine("/machine", onOut);
  const lang = (value) => run(async () => {
    const answer = await api("PATCH", "/machine/settings/general", { miniapp_language: value });
    setLang(value); onLang();
    return { change: answer.change };
  });
  return html`<${Screen} title=${m("machine")} back="#/" error=${error}>
    ${data && html`
      ${data.attention.length > 0 ? html`<section class="check">
        <div class="lab">${m("attention")}</div>
        ${data.attention.map((a) => a.screen
          ? html`<a class="cq" key=${a.text} href=${"#/" + a.screen}><span class="cq-text">${a.text}</span>
              ${icon(ICONS.next, 16)}</a>`
          : html`<div class="cq" key=${a.text}><span class="cq-text">${a.text}</span></div>`)}
      </section>` : html`<div class="note">${m("allWorks")}</div>`}
      <${Section} title=${m("sources")}>
        ${data.sources.map((s) => html`<${SourceRow} key=${s.key} s=${s} llm=${data.llm} />`)}
      <//>
      <${Section} title=${m("setup")} foot=${m("secrets")}>
        <${Go} href="#/machine/journal" title=${m("journal")} />
        ${data.has.twin && html`<${Go} href="#/machine/chats" title=${m("twin")} />`}
        ${data.has.hseapp && html`<${Go} href="#/machine/hseapp" title=${m("hseapp")} />`}
        ${data.has.grades && html`<${Go} href="#/machine/grades" title=${m("grades")} />`}
        ${data.has.settings && html`<${Go} href="#/machine/processing" title=${m("processing")} />`}
        <${Go} href="#/machine/courses" title=${m("courses")} />
        ${data.llm && html`<${Go} href="#/machine/llm" title=${m("llm")}
          value=${`${usd(data.llm.spent_usd)} / ${usd(data.llm.limit_usd)}`} bad=${data.llm.level === "hard"} />`}
        ${data.has.connector && html`<${Go} href="#/machine/connector" title=${m("connector")}
          value=${data.connections} />`}
        <${Go} href="#/machine/push" title=${m("notifications")} value=${data.push} />
        <${Go} href="#/machine/login" title=${m("loginDevices")} />
      <//>
      ${data.has.settings && html`<div class="langs">
        <button aria-pressed=${getLang() === "ru"} onClick=${() => lang("ru")}>Русский</button> ·
        <button aria-pressed=${getLang() === "en"} onClick=${() => lang("en")}>English</button>
      </div>`}`}
  <//>`;
}

// --- the journal ---------------------------------------------------------------------------------------------

export function Journal({ onOut }) {
  const { data, error, busy, run } = useMachine("/machine/journal", onOut);
  const undo = (c) => run(async () => {
    await api("POST", `/changes/${c.id}/undo`, {});
    toast(m("undoneNote"));
    changed();
    return null;
  });
  return html`<${Screen} title=${m("journal")} error=${error}>
    <div class="note">${m("journalHint")}</div>
    ${data && (data.changes.length === 0 ? html`<div class="empty">${m("noChanges")}</div>`
      : html`<div class="box list">${data.changes.map((c) => html`<div class="row" key=${c.id}>
          <div>
            <div class=${c.undone_at ? "mut" : ""}>${c.undone_at ? html`<s>${c.summary}</s>` : c.summary}</div>
            <div class="mut small">${fresh(c.at)} · ${c.via === "claude" ? m("viaClaude") : m("viaWeb")}
              ${c.undone_at ? ` · ${m("undone")} ${fresh(c.undone_at)}` : !c.undo ? ` · ${m("cannotUndo")}` : ""}</div>
            ${c.note && html`<div class="mut small">${c.note}</div>`}
            ${c.undo_error && !c.undone_at && html`<div class="err small">${c.undo_error}</div>`}
          </div>
          ${c.undo && !c.undone_at && html`<button class="sec" disabled=${busy} onClick=${() => undo(c)}>
            ${m("undo")}</button>`}
        </div>`)}</div>`)}
  <//>`;
}

// --- the twin and the chats --------------------------------------------------------------------------------

function TwinLogin({ twin, onTwin }) {
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const step = twin.login ? twin.login.step : "phone";
  const send = async (path, data) => {
    setBusy(true); setError("");
    try { onTwin(await api("POST", path, data)); setValue(""); } catch (e) { setError(e.message); }
    finally { setBusy(false); }
  };
  const submit = (event) => { event.preventDefault(); send("/machine/twin/login", { [step]: value }); };
  const out = () => { if (confirm(m("logOutTwin"))) send("/machine/twin/logout"); };
  if (twin.account) {
    return html`<${Section} title=${m("twinTitle")}>
      <div class="row"><div><b>${twin.account.name}</b>${twin.account.username ? ` · @${twin.account.username}` : ""}
        <div class=${twin.error ? "err small" : "mut small"}>${twin.error || m("twinReads")}</div></div>
        <button class="sec danger" disabled=${busy} onClick=${out}>${m("logOut")}</button></div>
      ${error && html`<div class="err">${error}</div>`}
    <//>`;
  }
  const hint = step === "code" ? m("codeHint", { phone: twin.login.phone }) : step === "password" ? m("passwordHint")
    : m("phoneHint");
  return html`<${Section} title=${m("twinLogin")} foot=${hint}>
    ${twin.error && html`<div class="err small" style="padding-top:10px">${twin.error}</div>`}
    <form class="acts" style="padding:10px 0" onSubmit=${submit}>
      <input class="field" value=${value} onInput=${(e) => setValue(e.target.value)} aria-label=${m(step)}
        placeholder=${step === "phone" ? "+7…" : step === "code" ? "12345" : m("password")}
        type=${step === "password" ? "password" : step === "phone" ? "tel" : "text"}
        inputmode=${step === "code" ? "numeric" : undefined}
        autocomplete=${step === "code" ? "one-time-code" : step === "password" ? "current-password" : "tel"} />
      <button class="btn" type="submit" disabled=${busy || !value.trim()}>
        ${step === "phone" ? m("getCode") : m("logIn")}</button>
    </form>
    ${twin.login && html`<button class="sec wide danger" style="margin-bottom:10px" disabled=${busy}
      onClick=${() => send("/machine/twin/cancel")}>${m("cancel")}</button>`}
    ${error && html`<div class="err" style="padding-bottom:10px">${error}</div>`}
  <//>`;
}

const KINDS = { user: ["private", "личный"], bot: ["bot", "бот"], group: ["group", "группа"],
                supergroup: ["supergroup", "супергруппа"], forum: ["forum", "форум"], channel: ["channel", "канал"] };
const kindOf = (kind) => (KINDS[kind] ? KINDS[kind][getLang() === "ru" ? 1 : 0] : kind);

const chatFacts = (chat) => [kindOf(chat.kind), chat.settings.study && m("study"),
  chat.settings.filter === "important" && m("importantOnly")].filter(Boolean).join(" · ");

export function Chats({ onOut }) {
  const { data, setData, error, busy, run } = useMachine("/machine/chats", onOut);
  const [query, setQuery] = useState("");
  const toggle = (chat, on) => run(async () => {
    const answer = await api("PATCH", `/machine/chats/${chat.id}`, { enabled: on });
    setData((old) => ({ ...old, chats: old.chats.map((c) => c.id === chat.id ? { ...c, settings: answer.settings } : c) }));
    return { change: answer.change };
  });
  const shown = data ? data.chats.filter((c) => c.title.toLowerCase().includes(query.trim().toLowerCase())) : [];
  const part = (on) => shown.filter((c) => c.settings.enabled === on);
  return html`<${Screen} title=${m("twin")} error=${error}>
    ${data && html`
      ${data.twin && html`<${TwinLogin} twin=${data.twin} onTwin=${(twin) => setData((old) => ({ ...old, twin }))} />`}
      ${data.chats.length >= 8 && html`<input class="field" type="search" placeholder=${m("findChat")} value=${query}
        onInput=${(e) => setQuery(e.target.value)} />`}
      ${[[true, m("chatsOn")], [false, m("chatsOff")]].map(([on, title]) => part(on).length > 0 && html`
        <${Section} key=${title} title=${`${title} · ${part(on).length}`}>
          ${part(on).map((c) => html`<div class="row" key=${c.id}>
            <a href=${`#/machine/chat?id=${c.id}`}><b>${c.title}</b><div class="mut small">${chatFacts(c)}</div></a>
            <${Switch} on=${c.settings.enabled} busy=${busy} label=${c.title} onChange=${(v) => toggle(c, v)} />
          </div>`)}
        <//>`)}
      ${shown.length === 0 && html`<div class="empty">${data.chats.length ? m("nothingFound") : m("chatsNone")}</div>`}`}
  <//>`;
}

const FLAGS = [["study", "flagStudy"], ["urgent", "flagUrgent"], ["images", "flagImages"], ["pdf_content", "flagPdf"]];
const BATCH_HOURS = [1, 2, 3, 4, 6, 8, 12, 24];

export function Chat({ id, onOut }) {
  const { data, setData, error, busy, run } = useMachine("/machine/chats", onOut);
  const [people, setPeople] = useState(null);
  useEffect(() => { api("GET", `/machine/chats/${id}/senders`).then(setPeople).catch(() => setPeople([])); }, [id]);
  const chat = data && data.chats.find((c) => String(c.id) === String(id));
  const change = (values) => run(async () => {
    const answer = await api("PATCH", `/machine/chats/${id}`, values);
    setData((old) => ({ ...old, chats: old.chats.map((c) => c.id === chat.id ? { ...c, settings: answer.settings } : c) }));
    return { change: answer.change };
  });
  if (!chat) return html`<${Screen} title=${m("twin")} back="#/machine/chats" error=${error} />`;
  const s = chat.settings;
  return html`<${Screen} title=${chat.title} back="#/machine/chats" error=${error}
      sub=${[kindOf(chat.kind), chat.username && "@" + chat.username].filter(Boolean).join(" · ")}>
    <${Section}>
      <div class="row"><div><b>${m("readChat")}</b></div>
        <${Switch} on=${s.enabled} busy=${busy} label=${m("readChat")} onChange=${(v) => change({ enabled: v })} /></div>
    <//>
    <${Section} title=${m("whatToDo")}>
      ${FLAGS.map(([name, key]) => html`<div class="row" key=${name}>
        <div><b>${m(key)}</b><div class="mut small">${m(key + "Hint")}</div></div>
        <${Switch} on=${s[name]} busy=${busy} label=${m(key)} onChange=${(v) => change({ [name]: v })} /></div>`)}
    <//>
    <${Section} title=${m("parsing")} foot=${m("importantHint")}>
      <div class="row"><div><b>${m("messages")}</b></div>
        <select class="field narrow" value=${s.filter} disabled=${busy} onChange=${(e) => change({ filter: e.target.value })}>
          <option value="all">${m("allMessages")}</option><option value="important">${m("importantOnly")}</option>
        </select></div>
      <div class="row"><div><b>${m("howOften")}</b></div>
        <select class="field narrow" value=${String(s.batch_hours ?? "")} disabled=${busy}
          onChange=${(e) => change({ batch_hours: e.target.value ? Number(e.target.value) : null })}>
          <option value="">${m("asAll")}</option>
          ${BATCH_HOURS.map((h) => html`<option value=${String(h)}>${m("everyH", { n: h })}</option>`)}
        </select></div>
    <//>
    <${Section} title=${m("noteForModel")} foot=${m("noteHint")}>
      <textarea class="field area" style="margin:10px 0" maxlength="300" disabled=${busy} value=${s.note}
        onChange=${(e) => change({ note: e.target.value })}></textarea>
    <//>
    <${Section} title=${m("people")} foot=${m("peopleHint")}>
      ${people && (people.length === 0 ? html`<div class="row mut">${m("nobody")}</div>`
        : people.map((p) => html`<div class="row" key=${p.id}>
          <div><b>${p.name || `id ${p.id}`}</b>
            <div class="mut small">${p.messages ? m("messagesN", { n: p.messages }) : m("notWritten")}</div></div>
          <${Switch} on=${s.people.includes(p.id)} busy=${busy} label=${p.name || p.id}
            onChange=${(v) => change({ people: v ? [...s.people, p.id] : s.people.filter((x) => x !== p.id) })} />
        </div>`))}
    <//>
  <//>`;
}

// --- HSE App X ----------------------------------------------------------------------------------------------

const LOGIN_ERROR = "login needed: ";

export function HseApp({ onOut }) {
  const { data, setData, error, busy, run, reload } = useMachine("/machine/hseapp", onOut);
  const [note, setNote] = useState("");
  const [fallback, setFallback] = useState(false);
  const [pasted, setPasted] = useState("");
  // After a login link is opened: the state every 3 s until the login is back, 5 minutes at most.
  const [watching, setWatching] = useState(0);
  useEffect(() => {
    if (!watching) return undefined;
    const timer = setInterval(async () => {
      if (Date.now() - watching > 300000) { setWatching(0); return; }
      try {
        const state = await api("GET", "/machine/hseapp");
        if (state.status.logged_in && data && !data.status.logged_in) setNote(m("hseLoggedIn"));
        if (state.status.logged_in) setWatching(0);
        setData(state);
      } catch { setWatching(0); }
    }, 3000);
    return () => clearInterval(timer);
  }, [watching]);
  // The window opens in the tap itself (a browser lets only a tap open one), the link comes into it after.
  const login = async (app) => {
    const opened = window.open("about:blank", "_blank");
    try {
      const { url } = await api("POST", "/machine/hseapp/link", { app });
      if (opened) opened.location.href = url; else location.href = url;
      setNote(m("hseOpened"));
      setWatching(Date.now());
    } catch (e) { if (opened) opened.close(); setNote(e.message); }
  };
  const save = () => run(async () => {
    const state = await api("POST", "/machine/hseapp/login", { url: pasted });
    setPasted(""); setFallback(false); setNote(m("hseLoggedIn"));
    return state;
  });
  const s = data && data.status;
  const ended = s && (s.error || "").startsWith(LOGIN_ERROR);
  return html`<${Screen} title=${m("hseapp")} sub=${m("hseIntro")} error=${error}>
    ${s && html`
      ${s.enabled && !s.logged_in && html`<div class="hint">${ended ? s.error : m("hseNeedsLogin")}</div>`}
      ${s.error && s.enabled && !ended && html`<div class="hint">${m("lastError")}${s.error}</div>`}
      <${Section} foot=${m("hseOne")}>
        <div class="row"><div><b>${m("hsePoll")}</b></div>
          <${Switch} on=${s.enabled} busy=${busy} label=${m("hsePoll")}
            onChange=${(v) => run(() => api("PATCH", "/machine/hseapp", { enabled: v }))} /></div>
      <//>
      ${s.enabled && s.logged_in && html`<${Section}>
        <div class="row"><div>${m("hseActive")}</div><b>${s.items ?? 0}</b></div>
        <div class="row"><div>${m("hseDone")}</div><b>${s.done ?? 0}</b></div>
        <div class="row"><div>${m("updated")}</div><b>${s.ok_at ? fresh(s.ok_at) : m("soon")}</b></div>
      <//>`}
      ${s.enabled && html`
        <button class="btn" onClick=${() => login(false)}>${s.logged_in ? m("logInAgain") : m("logIn")}</button>
        <div class="note">${note || m("hseOnce")}</div>
        <button class="sec wide" onClick=${() => setFallback(!fallback)} aria-expanded=${fallback}>
          ${m("hseNoReturn")}</button>
        ${fallback && html`<div class="stack">
          <button class="sec wide" onClick=${() => login(true)}>${m("hseAppAddress")}</button>
          <textarea class="field area" placeholder="https://…/hseapp/callback?state=…&code=…" value=${pasted}
            autocomplete="off" spellcheck="false" onInput=${(e) => setPasted(e.target.value)}></textarea>
          <button class="btn" disabled=${busy || !pasted.trim()} onClick=${save}>${m("hseSave")}</button>
          <div class="note small">${m("hseFallback", { url: data.callback })}</div>
        </div>`}`}`}
    ${!s && !error && html`<button class="sec wide" onClick=${reload}>…</button>`}
  <//>`;
}

// --- batches, news, reminders, PDFs, images ---------------------------------------------------------------------

export function Processing({ onOut }) {
  const { data, error, busy, run } = useMachine("/machine/settings", onOut);
  const set = (group, values) => run(() => api("PATCH", `/machine/settings/${group}`, values));
  const number = (text) => (text.trim() === "" ? NaN : Number(text));
  if (!data) return html`<${Screen} title=${m("processing")} error=${error} />`;
  const g = data.general;
  const wiki = data.wiki || {};
  return html`<${Screen} title=${m("processing")} error=${error}>
    <${Section} foot=${m("batchHint")}>
      <div class="row"><div><b>${m("batchEvery")}</b></div>
        <select class="field narrow" value=${String(g.batch_hours)} disabled=${busy}
          onChange=${(e) => set("general", { batch_hours: Number(e.target.value) })}>
          ${data.batch_choices.map((h) => html`<option value=${String(h)}>${m("everyH", { n: h })}</option>`)}
        </select></div>
      <div class="row"><div><b>${m("newsLives")}</b></div>
        <${Field} class="field narrow" type="number" min="1" max="168" inputmode="numeric" value=${g.news_ttl_hours}
          disabled=${busy} onSave=${(v) => set("general", { news_ttl_hours: number(v) })} /></div>
    <//>
    <${Section} title=${m("aboutMe")} foot=${m("aboutMeHint")}>
      <textarea class="field area" style="margin:10px 0" maxlength="500" disabled=${busy} value=${g.about_me}
        onChange=${(e) => set("general", { about_me: e.target.value })}></textarea>
    <//>
    <${Section} title=${m("wikiPages")} foot=${m("wikiHint")}>
      <textarea class="field area" style="margin:10px 0" disabled=${busy} spellcheck="false" autocomplete="off"
        placeholder="https://wiki…/Course" value=${g.wiki_pages.join("\n")}
        onChange=${(e) => set("general", { wiki_pages: e.target.value.split("\n").map((v) => v.trim()).filter(Boolean) })}
      ></textarea>
      ${g.wiki_pages.map((url) => wiki[url] && (wiki[url].error || wiki[url].title) && html`<div class="row" key=${url}>
        <div><b>${wiki[url].title || url}</b>
          <div class=${wiki[url].error ? "err small" : "mut small"}>${wiki[url].error || m("works", { n: wiki[url].items })}</div>
        </div></div>`)}
    <//>
    ${data.reminders && html`<${Section} title=${m("reminders")}>
      <div class="row"><div><b>${m("remindersOn")}</b></div>
        <${Switch} on=${data.reminders.enabled} busy=${busy} label=${m("remindersOn")}
          onChange=${(v) => set("reminders", { enabled: v })} /></div>
      ${data.reminders.enabled && html`
        <div class="row"><div><b>${m("eve")}</b></div>
          <${Field} class="field narrow" type="time" value=${data.reminders.eve} disabled=${busy}
            onSave=${(v) => v && set("reminders", { eve: v })} /></div>
        <div class="row"><div><b>${m("morning")}</b></div>
          <${Field} class="field narrow" type="time" value=${data.reminders.day} disabled=${busy}
            onSave=${(v) => v && set("reminders", { day: v })} /></div>`}
    <//>`}
    ${data.pdf && html`<${Section} title=${m("wholePdf")} foot=${m("pdfHint")}>
      <div class="row"><div><b>${m("pdfPages")}</b></div>
        <${Field} class="field narrow" type="number" min="1" max="50" inputmode="numeric" value=${data.pdf.pdf_pages}
          disabled=${busy} onSave=${(v) => set("pdf", { pdf_pages: number(v) })} /></div>
      <div class="row"><div><b>${m("pdfTokens")}</b></div>
        <${Field} class="field narrow" type="number" min="1000" max="200000" step="1000" inputmode="numeric"
          value=${data.pdf.pdf_tokens} disabled=${busy} onSave=${(v) => set("pdf", { pdf_tokens: number(v) })} /></div>
    <//>`}
    <${Section} title=${m("images")} foot=${m("imagesHint")}>
      <div class="row"><div><b>${m("imagesDaily")}</b>
          ${data.images.today !== undefined && html`<div class="mut small">${m("countImages", { n: data.images.today })}</div>`}</div>
        <${Field} class="field narrow" type="number" min="0" max="500" inputmode="numeric" value=${data.images.daily}
          disabled=${busy} onSave=${(v) => set("images", { daily: number(v) })} /></div>
    <//>
  <//>`;
}

// --- the course dictionary ---------------------------------------------------------------------------------

function EntryForm({ entry, limits, busy, onSave, onDelete }) {
  const [name, setName] = useState(entry ? entry.name : "");
  const [title, setTitle] = useState(entry ? entry.title : "");
  const [abbr, setAbbr] = useState(entry ? entry.abbr : "");
  const submit = (event) => { event.preventDefault(); onSave({ name: name.trim(), title: title.trim(), abbr: abbr.trim() }); };
  return html`<form class="stack" style="padding:10px 0" onSubmit=${submit}>
    ${!entry && html`<input class="field" placeholder=${m("name")} value=${name} required
      onInput=${(e) => setName(e.target.value)} />`}
    <input class="field" placeholder=${m("title")} value=${title} maxlength=${limits.title_chars} required
      onInput=${(e) => setTitle(e.target.value)} />
    <input class="field" placeholder=${m("abbr")} value=${abbr} maxlength=${limits.abbr_chars} required
      onInput=${(e) => setAbbr(e.target.value)} />
    <div class="acts">
      <button class="btn" type="submit" disabled=${busy}>${m("save")}</button>
      ${entry && entry.manual && html`<button class="sec wide danger" type="button" disabled=${busy}
        onClick=${onDelete}>${m("removeEntry")}</button>`}
    </div>
  </form>`;
}

export function Courses({ onOut }) {
  const { data, error, busy, run } = useMachine("/machine/courses", onOut);
  const [open, setOpen] = useState(null);  // an entry's name, "+" for a new one
  const save = (values) => run(async () => {
    const answer = await api("PUT", "/machine/courses", values);
    setOpen(null);
    return answer;
  });
  const remove = (name) => run(async () => {
    const answer = await api("POST", "/machine/courses/remove", { name });
    setOpen(null);
    return answer;
  });
  return html`<${Screen} title=${m("courses")} error=${error}>
    <div class="note">${m("coursesHint")}</div>
    ${data && html`
      ${data.error && html`<div class="hint">${data.error}</div>`}
      <div class="box list">
        ${data.subjects.map((s) => html`<div key=${s.name}>
          <button class="row plain" aria-expanded=${open === s.name} onClick=${() => setOpen(open === s.name ? null : s.name)}>
            <div><b>${s.title}</b> <span class="mut">${s.abbr}</span>${s.manual && html`<span class="tag">${m("yours")}</span>`}
              <div class="mut small">${s.name}</div></div></button>
          ${open === s.name && html`<${EntryForm} entry=${s} limits=${data} busy=${busy} onSave=${save}
            onDelete=${() => remove(s.name)} />`}
        </div>`)}
      </div>
      ${open === "+" ? html`<div class="box"><${EntryForm} limits=${data} busy=${busy} onSave=${save} /></div>`
        : html`<button class="sec wide" onClick=${() => setOpen("+")}>${icon(ICONS.plus, 18)}${m("addEntry")}</button>`}`}
  <//>`;
}

// --- the LLM limit -----------------------------------------------------------------------------------------

export function Llm({ onOut }) {
  const { data, error, busy, run } = useMachine("/machine/llm", onOut);
  if (!data) return html`<${Screen} title=${m("llm")} error=${error} />`;
  const week = data.periods.week, month = data.periods.month;
  const top = Math.max(...data.days.map((d) => d.cost_usd), data.daily_limit_usd, 0.01);
  return html`<${Screen} title=${m("llm")} error=${error}>
    <div class="stats">
      <div class=${"stat dark"}><span class="lab">${m("todaySpent")}</span><big>${usd(data.spent_usd)}</big>
        <small>/ ${usd(data.limit_usd)}</small></div>
      <div class="stat"><span class="lab">${m("week")}</span><big>${usd(week.cost_usd)}</big>
        <small>${m("forecast")}: ${usd(data.month_forecast_usd)}</small></div>
    </div>
    ${data.level !== "ok" && html`<div class="hint">${data.level === "hard" ? m("stopped") : "80 %"}</div>`}
    <${Section}>
      <div class="row"><div><b>${m("limit")}</b></div>
        <${Field} class="field narrow" type="number" min="0.01" step="0.01" inputmode="decimal"
          value=${data.daily_limit_usd} disabled=${busy}
          onSave=${(v) => run(() => api("PATCH", "/machine/llm", { daily_limit_usd: Number(v) }))} /></div>
      <div class="row"><button class="sec wide" disabled=${busy}
        onClick=${() => run(() => api("POST", "/machine/llm/raise", {}))}>${m("raiseToday")}</button></div>
    <//>
    <${Section} title=${m("days30")}>
      <div class="bars" role="img" aria-label=${m("days30")}>
        ${data.days.map((d) => html`<span key=${d.day} title=${`${d.day}: ${usd(d.cost_usd)}`}
          class=${d.stopped ? "stopped" : ""} style=${{ height: Math.max(2, d.cost_usd / top * 100) + "%" }}></span>`)}
      </div>
    <//>
    <${Section} title=${m("byTask")}>
      ${week.by_task.map((r) => html`<div class="row" key=${r.task}><div>${r.task}
        <div class="mut small">${m("runs", { n: r.runs })}</div></div><b>${usd(r.cost_usd)}</b></div>`)}
    <//>
    ${week.by_chat.length > 0 && html`<${Section} title=${m("byChat")}>
      ${week.by_chat.map((r) => html`<div class="row" key=${r.id}><div>${r.title}</div><b>${usd(r.cost_usd)}</b></div>`)}
    <//>`}
    <div class="note small">${m("month")}: ${usd(month.cost_usd)}</div>
  <//>`;
}

// --- the gradebooks ----------------------------------------------------------------------------------------

const bookName = (b) => b.name || b.title || b.label || b.id;

export function Gradebooks({ onOut }) {
  const { data, error, busy, run } = useMachine("/machine/grades", onOut);
  const [url, setUrl] = useState("");
  const [label, setLabel] = useState("");
  const set = (values) => run(() => api("PATCH", "/machine/settings/grades", values));
  const add = (event) => {
    event.preventDefault();
    run(async () => {
      const answer = await api("POST", "/machine/grades", { url: url.trim(), label: label.trim() });
      setUrl(""); setLabel("");
      return answer;
    });
  };
  const student = (value) => {
    if (data.student && data.sheets.length && value.trim() !== data.student && !confirm(m("studentAsk"))) return;
    set({ student: value.trim() });
  };
  if (!data) return html`<${Screen} title=${m("grades")} error=${error} />`;
  return html`<${Screen} title=${m("grades")} error=${error}>
    <${Section}>
      <div class="stack" style="padding:10px 0;gap:6px"><span class="lab">${m("student")}</span>
        <${Field} value=${data.student} disabled=${busy} onSave=${student} /></div>
      <div class="row"><div><b>${m("windowDays")}</b></div>
        <${Field} class="field narrow" type="number" min="1" max="60" inputmode="numeric" value=${data.window_days}
          disabled=${busy} onSave=${(v) => set({ window_days: Number(v) })} /></div>
    <//>
    ${data.sheets.length > 0 && html`<${Section}>
      ${data.sheets.map((b) => html`<a class="row" key=${b.id} href=${`#/machine/gradebook?id=${encodeURIComponent(b.id)}`}>
        <div><b>${bookName(b)}</b><div class=${b.error ? "err small" : "mut small"}>${b.error
          || [b.label, b.ok_at ? `${m("read")} ${fresh(b.ok_at)}` : m("notYet")].filter(Boolean).join(" · ")}</div></div>
        ${icon(ICONS.next, 16)}</a>`)}
    <//>`}
    <${Section} title=${m("addBook")}>
      <form class="stack" style="padding:10px 0" onSubmit=${add}>
        <input class="field" placeholder=${m("bookUrl")} value=${url} autocomplete="off" spellcheck="false" required
          onInput=${(e) => setUrl(e.target.value)} />
        <input class="field" placeholder=${m("bookLabel")} value=${label} maxlength=${data.label_chars}
          onInput=${(e) => setLabel(e.target.value)} />
        <button class="btn" type="submit" disabled=${busy || !url.trim()}>${m("addBook")}</button>
      </form>
    <//>
  <//>`;
}

function tabFacts(tab) {
  if (tab.by === "failed") return m("notMarked");
  if (tab.skip) return `${m("skipped")}${tab.reason ? ": " + tab.reason : ""}`;
  return m("works", { n: tab.works.length });
}

export function Gradebook({ id, onOut }) {
  const { data, error, busy, run } = useMachine(`/machine/grades/${encodeURIComponent(id)}`, onOut);
  const path = `/machine/grades/${encodeURIComponent(id)}`;
  const change = (values) => run(() => api("PATCH", path, values));
  const read = () => run(async () => {
    const answer = await api("POST", path + "/read", {});
    toast(m("reading"));
    setTimeout(changed, 5000);
    return answer;
  });
  const remove = () => {
    if (!confirm(m("removeBookAsk", { name: bookName(data) }))) return;
    run(async () => {
      const answer = await api("DELETE", path);
      go("/machine/grades");
      return { change: answer.change };
    });
  };
  if (!data) return html`<${Screen} title=${m("grades")} back="#/machine/grades" error=${error} />`;
  return html`<${Screen} title=${bookName(data)} back="#/machine/grades" error=${error}>
    ${data.error && html`<div class="hint">${data.error}</div>`}
    <${Section}>
      <div class="stack" style="padding:10px 0;gap:6px"><span class="lab">${m("title")}</span>
        <${Field} value=${data.name} maxlength="60" placeholder=${data.title || ""} disabled=${busy}
          onSave=${(v) => change({ name: v.trim() })} /></div>
      <div class="stack" style="padding:0 0 10px;gap:6px"><span class="lab">${m("bookLabel")}</span>
        <${Field} value=${data.label} maxlength=${data.label_chars} placeholder=${data.course} disabled=${busy}
          onSave=${(v) => change({ label: v.trim() })} /></div>
    <//>
    <${Section}>
      <div class="row"><div>${m("read")}</div><b>${data.ok_at ? fresh(data.ok_at) : m("notYet")}</b></div>
      <div class="row"><div>${m("student")}</div><b>${data.student || "—"}</b></div>
      <div class="acts" style="padding:10px 0">
        <button class="sec" disabled=${busy} onClick=${read}>${m("readNow")}</button>
        <a class="sec" href=${data.url} target="_blank" rel="noopener">${icon(ICONS.link, 16)}${m("open")}</a>
      </div>
    <//>
    <${Section} title=${m("tabs")}>
      ${data.tabs.length === 0 ? html`<div class="row mut">${m("noTabs")}</div>`
        : data.tabs.map((tab) => html`<a class="row" key=${tab.tab}
            href=${`#/machine/tab?id=${encodeURIComponent(id)}&tab=${encodeURIComponent(tab.tab)}`}>
            <div><b>${tab.tab}</b>${tab.by === "you" && html`<span class="tag">${m("yours")}</span>`}
              <div class=${tab.by === "failed" ? "err small" : "mut small"}>${tabFacts(tab)}</div></div>
            ${icon(ICONS.next, 16)}</a>`)}
    <//>
    <button class="sec wide danger" disabled=${busy} onClick=${remove}>${m("removeBook")}</button>
  <//>`;
}

const plain = (w) => ({ column: w.column, label: w.label, max: w.max, bonus_column: w.bonus_column || null });
const mark = (value) => (value === null || value === undefined ? "—" : String(Math.round(value * 100) / 100));

function WorkForm({ work, tab, budget, busy, onSave, onDrop }) {
  const free = tab.columns.filter((c) => !tab.works.some((w) => w.column === c.column) || (work && c.column === work.column));
  const [column, setColumn] = useState(work ? work.column : (free[0] || {}).column);
  const [label, setLabel] = useState(work ? work.label : "");
  const [max, setMax] = useState(String(work ? work.max : 10));
  const [bonus, setBonus] = useState(work && work.bonus_column || "");
  const describe = (c) => `${c.column} · ${c.header || "—"} · ${c.value}`;
  const submit = (event) => {
    event.preventDefault();
    onSave({ column, label: label.trim(), max: Number(max), bonus_column: bonus || null });
  };
  return html`<form class="stack" style="padding:10px 0" onSubmit=${submit}>
    <label class="lab">${m("column")}</label>
    <select class="field" value=${column} onChange=${(e) => setColumn(e.target.value)}>
      ${free.map((c) => html`<option value=${c.column}>${describe(c)}</option>`)}</select>
    <input class="field" placeholder="HW 3" maxlength=${budget} value=${label} required
      onInput=${(e) => setLabel(e.target.value)} />
    <label class="lab">${m("outOf")}</label>
    <input class="field" type="number" min="0.01" step="any" inputmode="decimal" value=${max} required
      onInput=${(e) => setMax(e.target.value)} />
    <label class="lab">${m("bonus")}</label>
    <select class="field" value=${bonus} onChange=${(e) => setBonus(e.target.value)}>
      <option value="">${m("none")}</option>
      ${tab.columns.filter((c) => c.column !== column).map((c) => html`<option value=${c.column}>${describe(c)}</option>`)}
    </select>
    <div class="acts">
      <button class="btn" type="submit" disabled=${busy || !label.trim() || !column}>${m("save")}</button>
      ${onDrop && html`<button class="sec wide danger" type="button" disabled=${busy} onClick=${onDrop}>
        ${m("removeWork")}</button>`}
    </div>
  </form>`;
}

export function Tab({ id, name, onOut }) {
  const path = `/machine/grades/${encodeURIComponent(id)}`;
  const { data, error, busy, run } = useMachine(path, onOut);
  const [open, setOpen] = useState(null);  // a work's column, "+" for a new one
  const tab = data && data.tabs.find((x) => x.tab === name);
  const back = `#/machine/gradebook?id=${encodeURIComponent(id)}`;
  if (!tab) return html`<${Screen} title=${name} back=${back} error=${error} />`;
  const tabPath = `${path}/tabs/${encodeURIComponent(name)}`;
  const save = (works, skip = tab.skip) => run(async () => {
    const answer = await api("PUT", tabPath, { skip, works: works.map(plain) });
    setOpen(null);
    return answer;
  });
  const reset = () => {
    if (confirm(m("backToHaikuAsk"))) run(() => api("DELETE", tabPath));
  };
  const others = (column) => tab.works.filter((w) => w.column !== column);
  return html`<${Screen} title=${tab.tab} back=${back} error=${error}
      sub=${`${bookName(data)} · ${tab.by === "you" ? m("byYou") : tab.by === "failed" ? m("notMarked") : m("byHaiku")}`}>
    <${Section}>
      <div class="row"><div><b>${m("readTab")}</b>${tab.skip && tab.reason && html`<div class="mut small">${tab.reason}</div>`}</div>
        <${Switch} on=${!tab.skip} busy=${busy} label=${m("readTab")} onChange=${(v) => save(tab.works, !v)} /></div>
    <//>
    ${!tab.skip && html`<${Section} foot=${m("yourRow", { n: tab.own_row })}>
      ${tab.works.map((w) => html`<div key=${w.column}>
        <button class="row plain" aria-expanded=${open === w.column} onClick=${() => setOpen(open === w.column ? null : w.column)}>
          <div><b>${w.label}</b><div class="mut small">${m("column")} ${w.column}${w.header ? ` · «${w.header}»` : ""}
            ${w.max !== 10 ? ` · ${m("outOf").toLowerCase()} ${w.max}` : ""}</div></div>
          <b>${w.raw === null ? "—" : w.max !== 10 ? `${w.raw} → ${mark(w.value)}` : mark(w.value)}</b></button>
        ${open === w.column && html`<${WorkForm} work=${w} tab=${tab} budget=${data.label_budget} busy=${busy}
          onSave=${(next) => save([...others(w.column), next])} onDrop=${() => save(others(w.column))} />`}
      </div>`)}
      ${open === "+" ? html`<${WorkForm} tab=${tab} budget=${data.label_budget} busy=${busy}
          onSave=${(next) => save([...tab.works, next])} />`
        : tab.columns.some((c) => !tab.works.some((w) => w.column === c.column)) && html`<div style="padding:10px 0">
          <button class="sec wide" onClick=${() => setOpen("+")}>${icon(ICONS.plus, 18)}${m("addColumn")}</button></div>`}
    <//>`}
    <${Section} title=${m("allColumns")}>
      ${tab.columns.map((c) => html`<div class="row" key=${c.column}><div>${c.header || "—"}
        <div class="mut small">${m("column")} ${c.column}</div></div><b>${c.value}</b></div>`)}
    <//>
    ${tab.by === "you" && html`<button class="sec wide danger" disabled=${busy} onClick=${reset}>${m("backToHaiku")}</button>`}
  <//>`;
}

// --- the Claude connector ----------------------------------------------------------------------------------

export function ConnectorScreen({ onOut }) {
  const { data, error, busy, run } = useMachine("/machine/connector", onOut);
  const copy = async () => {
    try { await navigator.clipboard.writeText(data.url); toast(m("copied")); } catch { /* the address is on screen */ }
  };
  return html`<${Screen} title=${m("connector")} error=${error}>
    ${data && html`
      <${Section} foot=${m("connectorUrl")}>
        <div class="row"><div style="overflow-wrap:anywhere"><b>${data.url}</b></div>
          <button class="sec" onClick=${copy}>${m("copy")}</button></div>
      <//>
      <${Section} title=${m("connections")} foot=${m("tools", data.tools)}>
        ${data.connections.length === 0 ? html`<div class="row mut">${m("noConnections")}</div>`
          : data.connections.map((c) => html`<div class="row" key=${c.id}>
            <div><b>${c.name}</b>${c.redirect ? html` <span class="mut">${c.redirect}</span>` : ""}
              <div class="mut small">${m("connected")} ${fresh(c.created_at)}${c.used_at ? ` · ${m("used")} ${fresh(c.used_at)}` : ""}</div></div>
            <button class="sec danger" disabled=${busy}
              onClick=${() => run(() => api("DELETE", `/machine/connector/${c.id}`))}>${m("revoke")}</button>
          </div>`)}
      <//>
      <a class="link" href="#/machine/journal">${m("journal")}<span>${icon(ICONS.next, 16)}</span></a>`}
  <//>`;
}

// --- what the notifications bring, and the quiet hours (16.6) ----------------------------------------------------

const KINDS = [["deadlines", "kindDeadlines"], ["reminders", "kindReminders"], ["failures", "kindFailures"],
               ["llm", "kindLlm"]];

export function NotificationPrefs({ onOut }) {
  const { data, error, busy, run } = useMachine("/machine/settings", onOut);
  if (!data) return error ? html`<div class="err">${error}</div>` : null;
  const n = data.notifications;
  const set = (values) => run(() => api("PATCH", "/machine/settings/notifications", values));
  const [from, to] = n.quiet ? n.quiet.split("-") : ["23:00", "08:00"];
  return html`
    <${Section} title=${m("kinds")}>
      ${KINDS.map(([kind, key]) => html`<div class="row" key=${kind}><div><b>${m(key)}</b></div>
        <${Switch} on=${n.kinds.includes(kind)} busy=${busy} label=${m(key)}
          onChange=${(v) => set({ kinds: v ? [...n.kinds, kind] : n.kinds.filter((k) => k !== kind) })} /></div>`)}
    <//>
    <${Section} title=${m("quiet")} foot=${m("quietHint")}>
      <div class="row"><div><b>${m("quietOn")}</b></div>
        <${Switch} on=${Boolean(n.quiet)} busy=${busy} label=${m("quietOn")}
          onChange=${(v) => set({ quiet: v ? `${from}-${to}` : "" })} /></div>
      ${n.quiet && html`<div class="row"><div><b>${m("from")}</b></div>
          <${Field} class="field narrow" type="time" value=${from} disabled=${busy}
            onSave=${(v) => v && set({ quiet: `${v}-${to}` })} /></div>
        <div class="row"><div><b>${m("to")}</b></div>
          <${Field} class="field narrow" type="time" value=${to} disabled=${busy}
            onSave=${(v) => v && set({ quiet: `${from}-${v}` })} /></div>`}
    <//>
    ${error && html`<div class="err">${error}</div>`}`;
}
