import { BOT_NAME_LENGTH, WORK_TEXT_LENGTH } from "@persona/agent/work/limits";
import { Button, Switch } from "@persona/ui/controls";
import { ConfirmDialog } from "@persona/ui/dialog";
import { Checkbox, TextField } from "@persona/ui/field";
import { ActionMenu } from "@persona/ui/menu";
import { ChoicePicker } from "@persona/ui/picker";
import { useState } from "react";
import { client } from "../../../client.ts";
import type { useChat } from "../../chat/use-chat.ts";
import { WorkPanel } from "../../chat/work.tsx";
import { useI18n } from "../../i18n/context.tsx";
import { RoutineSchedule, routineTimeLabel } from "./routine-schedule.tsx";
import type { PanelEditor } from "./types.ts";
import type { WorkAction, WorkData } from "./use-bot-work.ts";

type Routine = WorkData["routines"]["items"][number];
const runLabelsZh = {
  queued: "排队中",
  running: "执行中",
  waiting_answer: "等待回答",
  waiting_approval: "等待审批",
  done: "已完成",
  failed: "失败",
  stopped: "已停止",
};
const runLabelsPt = {
  queued: "Na fila",
  running: "Em execução",
  waiting_answer: "Aguardando resposta",
  waiting_approval: "Aguardando aprovação",
  done: "Concluído",
  failed: "Falhou",
  stopped: "Parado",
};

function localTime(iso: string) {
  const date = new Date(iso);
  const parts = [
    date.getMonth() + 1,
    date.getDate(),
    date.getHours(),
    date.getMinutes(),
  ].map((part) => String(part).padStart(2, "0"));
  return `${date.getFullYear()}-${parts[0]}-${parts[1]}T${parts[2]}:${parts[3]}`;
}

function scheduleLabel(routine: Routine) {
  return routine.schedule.type === "once"
    ? new Date(routine.schedule.at).toLocaleString()
    : `${routineTimeLabel(routine.schedule.expression)} · ${routine.schedule.timeZone}`;
}

export function RoutinesPanel({
  botId,
  data,
  skills,
  action,
  editor,
  onEditorChange,
  chat,
}: {
  chat: ReturnType<typeof useChat>;
  botId: number;
  data: WorkData["routines"];
  skills: WorkData["skills"];
  action: WorkAction;
  editor: Extract<PanelEditor, { view: "routines" }> | null;
  onEditorChange: (editor: PanelEditor | null) => void;
}) {
  const { t, language } = useI18n();
  const isPt = language === "pt-BR";
  const runLabels = isPt ? runLabelsPt : runLabelsZh;
  const [selectedRun, setSelectedRun] = useState<string>();
  const [deleting, setDeleting] = useState<Routine | null>(null);
  const runs = [...data.runs].reverse();
  return (
    <section className="detail-page" aria-label={t("定时")}>
      {!editor && (
        <header className="detail-page-head">
          <span className="detail-note">
            {isPt
              ? `${data.items.length} rotinas`
              : `${data.items.length} 个计划`}
          </span>
          <Button
            size="small"
            disabled={action.busy}
            onClick={() =>
              onEditorChange({
                view: "routines",
                routine: null,
                token: crypto.randomUUID(),
              })
            }
          >
            {isPt ? "Nova rotina" : "新建定时任务"}
          </Button>
        </header>
      )}

      {editor && (
        <RoutineEditor
          key={editor.token}
          botId={botId}
          routine={editor.routine}
          skills={skills}
          action={action}
          onCancel={() => onEditorChange(null)}
          onSaved={() => onEditorChange(null)}
        />
      )}

      {!editor &&
        (data.items.length ? (
          <ul className="item-list">
            {data.items.map((routine) => (
              <li key={routine.id} className="item-card">
                <div className="item-heading">
                  <button
                    type="button"
                    className="item-heading-copy item-open"
                    onClick={() =>
                      onEditorChange({
                        view: "routines",
                        routine,
                        token: crypto.randomUUID(),
                      })
                    }
                  >
                    <h3>{routine.title}</h3>
                  </button>
                  <Switch
                    label={
                      isPt ? `Ativar ${routine.title}` : `启用 ${routine.title}`
                    }
                    checked={routine.enabled}
                    disabled={action.busy}
                    onCheckedChange={(next) =>
                      void action.run(
                        ["routines.enable", routine.id, routine.revision, next],
                        (requestId) =>
                          client.work.routines.enable({
                            botId,
                            id: routine.id,
                            enabled: next,
                            revision: routine.revision,
                            requestId,
                          }),
                      )
                    }
                  />
                </div>
                <div className="item-meta">
                  <span
                    className="item-tag"
                    data-tone={routine.enabled ? "ready" : undefined}
                  >
                    {routine.enabled
                      ? isPt
                        ? "Ativo"
                        : "已启用"
                      : isPt
                        ? "Pausado"
                        : "已暂停"}
                  </span>
                  <span>{scheduleLabel(routine)}</span>
                </div>
                {routine.nextAt && (
                  <p className="item-note">
                    {isPt
                      ? `Próxima: ${new Date(routine.nextAt).toLocaleString()}`
                      : `下次：${new Date(routine.nextAt).toLocaleString()}`}
                  </p>
                )}
                <div className="item-actions">
                  <ActionMenu
                    label={
                      isPt
                        ? `Ações de ${routine.title}`
                        : `${routine.title}的操作`
                    }
                    disabled={action.busy}
                    items={[
                      {
                        label: isPt ? "Testar" : "试跑",
                        onSelect: () =>
                          void action.run(
                            ["routines.test", routine.id],
                            (requestId) =>
                              client.work.routines.test({
                                botId,
                                id: routine.id,
                                requestId,
                              }),
                          ),
                      },
                      {
                        label: isPt ? "Editar" : "编辑",
                        onSelect: () =>
                          onEditorChange({
                            view: "routines",
                            routine,
                            token: crypto.randomUUID(),
                          }),
                      },
                      {
                        label: isPt ? "Excluir" : "删除",
                        tone: "danger",
                        onSelect: () => setDeleting(routine),
                      },
                    ]}
                  />
                </div>
              </li>
            ))}
          </ul>
        ) : (
          <p className="detail-empty">
            {isPt ? "Nenhuma rotina configurada" : "暂无定时任务"}
          </p>
        ))}

      {!editor && (
        <details className="detail-disclosure">
          <summary>
            {isPt ? "Histórico de execução" : "运行记录"}
            {runs.length > 0 && (
              <span className="disclosure-count">{runs.length}</span>
            )}
          </summary>
          <div className="detail-disclosure-body">
            {runs.length ? (
              <ul className="run-list">
                {runs.map((run) => (
                  <li key={run.id}>
                    <div className="run-heading">
                      <strong>{run.title}</strong>
                      <span>
                        {run.mode === "test"
                          ? isPt
                            ? "Teste"
                            : "试跑"
                          : isPt
                            ? "Rotina"
                            : "定时"}
                      </span>
                    </div>
                    <p className="item-note">{runLabels[run.status]}</p>
                    <p className="item-note">
                      {isPt
                        ? `Agendado: ${new Date(run.scheduledAt).toLocaleString()}; Disparado: ${new Date(run.firedAt).toLocaleString()}`
                        : `计划：${new Date(run.scheduledAt).toLocaleString()}；触发：${new Date(run.firedAt).toLocaleString()}`}
                    </p>
                    <p className="item-note">
                      {isPt
                        ? `Execução: ${((run.activeMs ?? 0) / 1000).toFixed(1)}s · Espera: ${((run.waitingMs ?? 0) / 1000).toFixed(1)}s · Limite: ${Math.round((run.executionMs ?? 900_000) / 60_000)} min`
                        : `执行 ${((run.activeMs ?? 0) / 1000).toFixed(1)} 秒 · 等人 ${((run.waitingMs ?? 0) / 1000).toFixed(1)} 秒 · 期限 ${Math.round((run.executionMs ?? 900_000) / 60_000)} 分钟`}
                    </p>
                    {run.turnId && (
                      <Button
                        variant="ghost"
                        size="small"
                        onClick={() => setSelectedRun(run.turnId)}
                      >
                        {isPt ? "Ver detalhes da execução" : "查看工作记录"}
                      </Button>
                    )}
                    {run.coalesced && (
                      <p className="item-note">
                        {isPt
                          ? "Períodos perdidos durante o desligamento foram consolidados nesta execução."
                          : "已将停机期间错过的周期合并为本次执行。"}
                      </p>
                    )}
                    {run.error && <p role="status">{run.error}</p>}
                    {run.status === "done" && (
                      <p className="item-note">
                        {run.delivery === "conditional"
                          ? run.delivered
                            ? isPt
                              ? "Entregue"
                              : "已交付"
                            : isPt
                              ? "Concluído em silêncio"
                              : "静默完成"
                          : isPt
                            ? "Execução concluída. Veja no chat e no registro de trabalho."
                            : "执行完成，查看聊天与工作记录。"}
                      </p>
                    )}
                  </li>
                ))}
              </ul>
            ) : (
              <p className="detail-note">
                {isPt
                  ? "Ainda não há registros de execução."
                  : "还没有运行记录。"}
              </p>
            )}
          </div>
        </details>
      )}

      {selectedRun && (
        <WorkPanel
          {...chat}
          botId={botId}
          selectedRun={selectedRun}
          onRunClose={() => setSelectedRun(undefined)}
          detailOnly
        />
      )}
      <ConfirmDialog
        open={Boolean(deleting)}
        onOpenChange={(open) => {
          if (!open) setDeleting(null);
        }}
        title={isPt ? "Excluir esta rotina?" : "删除这个定时任务？"}
        description={
          isPt
            ? "Tarefas já iniciadas ainda serão concluídas."
            : "已开始的工作仍会完成。"
        }
        onConfirm={() => {
          const routine = deleting;
          setDeleting(null);
          if (routine)
            void action.run(
              ["routines.delete", routine.id, routine.revision],
              () =>
                client.work.routines.delete({
                  botId,
                  id: routine.id,
                  revision: routine.revision,
                }),
              () => {
                if (editor?.routine?.id === routine.id) onEditorChange(null);
              },
            );
        }}
      />
    </section>
  );
}

function RoutineEditor({
  botId,
  routine,
  skills,
  action,
  onCancel,
  onSaved,
}: {
  botId: number;
  routine: Routine | null;
  skills: WorkData["skills"];
  action: WorkAction;
  onCancel: () => void;
  onSaved: () => void;
}) {
  const { language } = useI18n();
  const isPt = language === "pt-BR";
  const [id] = useState(() => routine?.id ?? crypto.randomUUID());
  const [title, setTitle] = useState(routine?.title ?? "");
  const [instructions, setInstructions] = useState(routine?.instructions ?? "");
  const [type, setType] = useState<"once" | "cron">(
    routine?.schedule.type ?? "cron",
  );
  const [at, setAt] = useState(
    routine?.schedule.type === "once" ? localTime(routine.schedule.at) : "",
  );
  const [expression, setExpression] = useState(
    routine?.schedule.type === "cron"
      ? routine.schedule.expression
      : "0 9 * * *",
  );
  const localZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const [timeZone, setTimeZone] = useState(
    routine?.schedule.timeZone ?? localZone,
  );
  const [minutes, setMinutes] = useState(
    String((routine?.executionMs ?? 900_000) / 60_000),
  );
  const [delivery, setDelivery] = useState<"always" | "conditional">(
    routine?.delivery ?? "always",
  );
  const [condition, setCondition] = useState(routine?.condition ?? "");
  const [skillIds, setSkillIds] = useState(routine?.skillIds ?? []);
  return (
    <form
      className="bot-form"
      aria-busy={action.busy}
      onSubmit={(event) => {
        event.preventDefault();
        const schedule =
          type === "once"
            ? { type, at: new Date(at).toISOString(), timeZone: localZone }
            : { type, expression, timeZone };
        const input = {
          botId,
          id,
          revision: routine?.revision ?? 0,
          title,
          instructions,
          delivery,
          condition: delivery === "conditional" ? condition : undefined,
          schedule,
          skillIds,
          executionMs: Math.round(Number(minutes) * 60_000),
        };
        void action.run(
          ["routines.save", input],
          (requestId) => client.work.routines.save({ ...input, requestId }),
          onSaved,
        );
      }}
    >
      <fieldset className="bot-fields" disabled={action.busy}>
        <div className="detail-form-head">
          <h3>
            {routine
              ? isPt
                ? "Editar rotina"
                : "编辑定时任务"
              : isPt
                ? "Nova rotina"
                : "新建定时任务"}
          </h3>
        </div>
        <TextField
          label={
            routine
              ? isPt
                ? "Nome da rotina"
                : "修改任务名称"
              : isPt
                ? "Nome da nova rotina"
                : "新任务名称"
          }
          value={title}
          onValueChange={setTitle}
          required
          maxLength={BOT_NAME_LENGTH}
        />
        <TextField
          label={
            isPt ? "Instruções e formato de entrega" : "工作要求与交付方式"
          }
          value={instructions}
          onValueChange={setInstructions}
          multiline
          required
          maxLength={WORK_TEXT_LENGTH}
        />
        <label>
          {isPt ? "Modo de entrega" : "交付方式"}
          <select
            className="field-control"
            value={delivery}
            onChange={(event) =>
              setDelivery(event.target.value as typeof delivery)
            }
          >
            <option value="always">
              {isPt ? "Sempre entregar" : "每次交付"}
            </option>
            <option value="conditional">
              {isPt
                ? "Notificar apenas se atender à condição"
                : "满足条件才通知"}
            </option>
          </select>
        </label>
        {delivery === "conditional" && (
          <TextField
            label={isPt ? "Condição de notificação" : "通知条件"}
            value={condition}
            onValueChange={setCondition}
            required
            maxLength={WORK_TEXT_LENGTH}
          />
        )}
        <TextField
          label={
            isPt
              ? "Tempo limite (minutos, sem tempo de espera)"
              : "执行期限（分钟，不含等人时间）"
          }
          value={minutes}
          onValueChange={setMinutes}
          required
        />
        <div className="field">
          <span className="field-label">
            {isPt ? "Tipo de agendamento" : "执行方式"}
          </span>
          <ChoicePicker
            label={isPt ? "Tipo de agendamento" : "执行方式"}
            value={type}
            onValueChange={(value) => setType(value as "once" | "cron")}
            items={[
              { value: "cron", label: isPt ? "Recorrente" : "周期执行" },
              { value: "once", label: isPt ? "Uma única vez" : "执行一次" },
            ]}
          />
        </div>
        {type === "once" ? (
          <label className="field">
            <span className="field-label">
              {isPt
                ? `Horário de execução (${localZone})`
                : `执行时间（${localZone}）`}
            </span>
            <input
              className="field-control"
              type="datetime-local"
              value={at}
              onChange={(event) => setAt(event.target.value)}
              required
            />
          </label>
        ) : (
          <RoutineSchedule
            expression={expression}
            onExpressionChange={setExpression}
            timeZone={timeZone}
            onTimeZoneChange={setTimeZone}
          />
        )}
        {skills
          .filter((skill) => skill.enabledBots.includes(botId))
          .map((skill) => (
            <Checkbox
              key={skill.id}
              label={skill.name}
              checked={skillIds.includes(skill.id)}
              disabled={action.busy}
              onCheckedChange={(checked) =>
                setSkillIds(
                  checked
                    ? [...skillIds, skill.id]
                    : skillIds.filter((id) => id !== skill.id),
                )
              }
            />
          ))}
        <div className="form-actions">
          <Button type="button" variant="ghost" size="small" onClick={onCancel}>
            {isPt ? "Cancelar" : "取消"}
          </Button>
          <Button
            type="submit"
            size="small"
            aria-label={isPt ? "Salvar rotina" : "保存为暂停"}
          >
            {isPt ? "Salvar" : "保存"}
          </Button>
        </div>
      </fieldset>
    </form>
  );
}
