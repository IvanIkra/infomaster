// The app's texts in both languages (D41). t(key, ...args): a string, or a function of args for the few with values.

const TEXTS = {
  tagline: ["Deadlines, classes and materials from chats, mail and the timetable — in one place.",
            "Сроки, пары и материалы из чатов, почты и РУЗ — в одном месте."],
  passkeyLogin: ["Log in with a passkey", "Войти по ключу доступа"],
  passkeyHint: ["Face ID, Touch ID or a key on another device", "Face ID, Touch ID или ключ на другом устройстве"],
  newDevice: ["new device", "новое устройство"],
  codeIntro: ["The bot will send six digits to your chat with it. The code works for 10 minutes.",
              "Бот пришлёт шесть цифр в чат с ним. Код действует 10 минут."],
  sendCode: ["Send a code", "Прислать код"],
  sendAgain: ["Send another code", "Прислать новый код"],
  codeSent: ["The code is in your chat with the bot.", "Код — в чате с ботом."],
  codeLabel: ["Code from the bot", "Код от бота"],
  logIn: ["Log in", "Войти"],
  offerTitle: ["A passkey for this device?", "Ключ доступа на этом устройстве?"],
  offerText: ["Next time you will log in with Face ID, Touch ID or the device's PIN, without a code from the bot.",
              "В следующий раз войдёте по Face ID, Touch ID или PIN устройства, без кода от бота."],
  createPasskey: ["Create a passkey", "Создать ключ"],
  later: ["Later", "Позже"],
  feedSoon: ["The feed comes next", "Лента — на следующем шаге"],
  feedSoonText: ["Classes and deadlines will be here. For now they are in the Mini App.",
                 "Здесь будут пары и сроки. Пока они в Mini App."],
  installIos: ["For notifications on iPhone, add the app to the Home Screen: Share → Add to Home Screen.",
               "Чтобы на iPhone приходили уведомления, добавьте приложение на экран «Домой»: Поделиться → На экран «Домой»."],
  machine: ["Machine", "Машина"],
  machineOk: ["Machine: all works", "Машина: всё работает"],
  back: ["Back", "Назад"],
  login: ["Login and devices", "Вход и устройства"],
  devices: ["Devices", "Устройства"],
  thisDevice: ["this device", "это устройство"],
  lastUsed: ["used", "заходили"],
  added: ["added", "добавлен"],
  endSession: ["Log out there", "Выйти там"],
  passkeys: ["Passkeys", "Ключи доступа"],
  noPasskeys: ["No passkeys yet: every login needs a code from the bot.",
               "Ключей пока нет: каждый вход — по коду от бота."],
  addPasskey: ["Add a passkey on this device", "Добавить ключ на этом устройстве"],
  remove: ["Remove", "Удалить"],
  logOut: ["Log out", "Выйти"],
  failed: ["Something went wrong. Try again.", "Что-то пошло не так. Попробуйте ещё раз."],
  cancelled: ["The passkey request was cancelled.", "Запрос ключа отменён."],
  offline: ["No connection to infomaster.", "Нет связи с infomaster."],
  search: ["Search", "Поиск"],
};

let lang = "en";

export function setLang(value) { lang = value === "ru" ? "ru" : "en"; document.documentElement.lang = lang; }
export function getLang() { return lang; }
export function t(key) { const pair = TEXTS[key]; return pair ? pair[lang === "ru" ? 1 : 0] : key; }

export function when(seconds) {
  // '20 Nov, 12:45' or '20 нояб., 12:45' for a unix time, in the device's zone
  const date = new Date(seconds * 1000);
  return date.toLocaleString(lang === "ru" ? "ru-RU" : "en-GB",
                             { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}

export function today() {
  const date = new Date();
  const locale = lang === "ru" ? "ru-RU" : "en-GB";
  return { weekday: date.toLocaleDateString(locale, { weekday: "long" }),
           day: date.toLocaleDateString(locale, { day: "numeric", month: "long" }) };
}
