import { getLanguage } from "../i18n/context.tsx";

function getLocale() {
  const lang = getLanguage();
  return lang === "zh-CN" ? "zh-CN" : "pt-BR";
}

function getFormatters() {
  const locale = getLocale();
  return {
    clock: new Intl.DateTimeFormat(locale, {
      hourCycle: "h23",
      hour: "2-digit",
      minute: "2-digit",
    }),
    shortDate: new Intl.DateTimeFormat(locale, {
      month: "long",
      day: "numeric",
    }),
    longDate: new Intl.DateTimeFormat(locale, {
      year: "numeric",
      month: "long",
      day: "numeric",
    }),
    weekday: new Intl.DateTimeFormat(locale, { weekday: "short" }),
  };
}

const TIME_BREAK_MS = 5 * 60_000;

export function clockTime(at: number) {
  return getFormatters().clock.format(at);
}

function startOfDay(at: number) {
  const date = new Date(at);
  return new Date(
    date.getFullYear(),
    date.getMonth(),
    date.getDate(),
  ).getTime();
}

function dayDifference(from: number, to: number) {
  return Math.round((startOfDay(to) - startOfDay(from)) / 86_400_000);
}

export function timeBreak(
  previousAt: number | undefined,
  currentAt: number | undefined,
  now = Date.now(),
) {
  if (currentAt === undefined || !Number.isFinite(currentAt)) return null;
  if (previousAt !== undefined && Number.isFinite(previousAt)) {
    const sameDay = dayDifference(previousAt, currentAt) === 0;
    if (sameDay && currentAt - previousAt < TIME_BREAK_MS) return null;
  }
  const time = clockTime(currentAt);
  const age = dayDifference(currentAt, now);
  const isZh = getLanguage() === "zh-CN";

  if (age <= 0) return isZh ? `今天 ${time}` : `Hoje ${time}`;
  if (age === 1) return isZh ? `昨天 ${time}` : `Ontem ${time}`;
  const date = new Date(currentAt);
  const { shortDate, longDate, weekday } = getFormatters();
  const dateLabel =
    date.getFullYear() === new Date(now).getFullYear()
      ? shortDate.format(date)
      : longDate.format(date);
  return `${dateLabel} ${weekday.format(date)} ${time}`;
}

export function contactTime(at: number, now = Date.now()) {
  const age = dayDifference(at, now);
  const isZh = getLanguage() === "zh-CN";
  if (age <= 0) return clockTime(at);
  if (age === 1) return isZh ? "昨天" : "Ontem";
  const { weekday, longDate } = getFormatters();
  if (age < 7) return weekday.format(at);
  return longDate.format(at);
}
