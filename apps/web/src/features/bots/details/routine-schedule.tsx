import {
  SCHEDULE_EXPRESSION_LENGTH,
  TIME_ZONE_LENGTH,
} from "@persona/agent/work/limits";
import { days, expressionPattern } from "@persona/agent/work/schedule-label";
import { Button } from "@persona/ui/controls";
import { Dialog } from "@persona/ui/dialog";
import { TextField } from "@persona/ui/field";
import { ChoicePicker } from "@persona/ui/picker";
import { useState } from "react";
import { useI18n } from "../../i18n/context.tsx";

const daysPt = [
  "Domingo",
  "Segunda-feira",
  "Terça-feira",
  "Quarta-feira",
  "Quinta-feira",
  "Sexta-feira",
  "Sábado",
];

export function routineTimeLabel(expression: string) {
  const match = expressionPattern.exec(expression);
  if (!match) return expression;
  const [, minute = "0", hour = "9", weekday = "*"] = match;
  const isPt =
    typeof document !== "undefined" &&
    document.documentElement.lang !== "zh-CN";
  if (isPt) {
    const ptDays = [
      "Domingo",
      "Segunda",
      "Terça",
      "Quarta",
      "Quinta",
      "Sexta",
      "Sábado",
    ];
    const freq =
      weekday === "*"
        ? "Todos os dias"
        : weekday === "1-5"
          ? "Dias úteis"
          : `Todo(a) ${ptDays[Number(weekday)]}`;
    return `${freq} às ${hour.padStart(2, "0")}:${minute.padStart(2, "0")}`;
  }
  const frequency =
    weekday === "*"
      ? "每天"
      : weekday === "1-5"
        ? "工作日"
        : `每${days[Number(weekday)]}`;
  return `${frequency} ${hour.padStart(2, "0")}:${minute.padStart(2, "0")}`;
}

export function RoutineSchedule({
  expression,
  onExpressionChange,
  timeZone,
  onTimeZoneChange,
}: {
  expression: string;
  onExpressionChange: (value: string) => void;
  timeZone: string;
  onTimeZoneChange: (value: string) => void;
}) {
  const { t, language } = useI18n();
  const isPt = language === "pt-BR";
  const frequencies = [
    { value: "daily", label: isPt ? "Diariamente" : "每天" },
    { value: "weekdays", label: isPt ? "Dias úteis" : "工作日" },
    { value: "weekly", label: isPt ? "Semanalmente" : "每周" },
    { value: "custom", label: isPt ? "Personalizado" : "自定义" },
  ];
  const match = expressionPattern.exec(expression);
  const [, minute = "0", hour = "9", weekday = "*"] = match ?? [];
  const [frequency, setFrequency] = useState(
    !match
      ? "custom"
      : weekday === "*"
        ? "daily"
        : weekday === "1-5"
          ? "weekdays"
          : "weekly",
  );
  const [time, setTime] = useState(
    `${hour.padStart(2, "0")}:${minute.padStart(2, "0")}`,
  );
  const [day, setDay] = useState(/^[0-6]$/.test(weekday) ? weekday : "1");
  const [zoneOpen, setZoneOpen] = useState(false);
  function update(nextFrequency: string, nextTime: string, nextDay: string) {
    setFrequency(nextFrequency);
    setTime(nextTime);
    setDay(nextDay);
    if (nextFrequency === "custom" || !nextTime) return;
    const [hour, minute] = nextTime.split(":").map(Number);
    const weekday =
      nextFrequency === "daily"
        ? "*"
        : nextFrequency === "weekdays"
          ? "1-5"
          : nextDay;
    onExpressionChange(`${minute} ${hour} * * ${weekday}`);
  }
  return (
    <>
      <div className="field">
        <span className="field-label">
          {isPt ? "Horário de repetição" : "重复时间"}
        </span>
        <div className="schedule-row">
          <ChoicePicker
            label={isPt ? "Frequência" : "重复频率"}
            items={frequencies}
            value={frequency}
            onValueChange={(value) => update(value, time, day)}
          />
          {frequency === "weekly" && (
            <ChoicePicker
              label={isPt ? "Dia da semana" : "星期"}
              items={(isPt ? daysPt : days).map((label, value) => ({
                label,
                value: String(value),
              }))}
              value={day}
              onValueChange={(value) => update(frequency, time, value)}
            />
          )}
          {frequency !== "custom" && (
            <input
              className="field-control"
              aria-label={isPt ? "Horário de execução" : "每天执行时间"}
              type="time"
              value={time}
              required
              onChange={(event) => update(frequency, event.target.value, day)}
            />
          )}
        </div>
      </div>
      {frequency === "custom" && (
        <TextField
          label={isPt ? "Expressão cron personalizada" : "自定义时间规则"}
          value={expression}
          onValueChange={onExpressionChange}
          required
          maxLength={SCHEDULE_EXPRESSION_LENGTH}
        />
      )}
      <div className="detail-actions">
        <span className="detail-note">
          {isPt
            ? `Executado no fuso horário ${timeZone}`
            : `按 ${timeZone} 时间执行`}
        </span>
        <Button
          type="button"
          variant="ghost"
          size="small"
          onClick={() => setZoneOpen(true)}
        >
          {isPt ? "Alterar fuso" : "更改时区"}
        </Button>
      </div>
      <Dialog
        open={zoneOpen}
        onOpenChange={setZoneOpen}
        title={isPt ? "Fuso horário de execução" : "执行时区"}
        description={
          isPt
            ? "Informe o fuso horário para a rotina."
            : "填写任务使用的时区。"
        }
        footer={<Button onClick={() => setZoneOpen(false)}>{t("完成")}</Button>}
      >
        <TextField
          label={isPt ? "Fuso horário" : "时区"}
          value={timeZone}
          onValueChange={onTimeZoneChange}
          required
          maxLength={TIME_ZONE_LENGTH}
        />
      </Dialog>
    </>
  );
}
