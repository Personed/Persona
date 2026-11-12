import type { EngineAnswer } from "@persona/agent/harness/types";
import type { ChatMessage } from "@persona/agent/messages";
import { Button } from "@persona/ui/controls";
import { TextField } from "@persona/ui/field";
import { InlineNotice } from "@persona/ui/message";
import { QuestionIcon } from "@phosphor-icons/react/dist/csr/Question";
import { ShieldCheckIcon } from "@phosphor-icons/react/dist/csr/ShieldCheck";
import { useState } from "react";
import { client } from "../../client.ts";
import { useWorkAction } from "../bots/details/use-bot-work.ts";
import { useI18n } from "../i18n/context.tsx";

export function Approval({
  botId,
  message,
}: {
  botId: number;
  message: ChatMessage;
}) {
  const { language } = useI18n();
  const isPt = language === "pt-BR";
  const record = message.approval;
  const request = record?.request;
  const [text, setText] = useState("");
  const [custom, setCustom] = useState<string>();
  const [answers, setAnswers] = useState<Record<string, string[]>>(() =>
    Object.fromEntries(
      (request?.questions ?? []).map((question) => [
        question.id,
        Array.isArray(question.value)
          ? question.value
          : question.value
            ? [question.value]
            : [],
      ]),
    ),
  );
  const action = useWorkAction();
  if (!record || !request) return null;
  const pending = record.state === "pending";
  const question = request.kind === "question";
  const single =
    request.questions?.length === 1 && !request.questions[0]?.multiple;
  const respond = (answer: EngineAnswer) =>
    action.run([request.id, answer], () =>
      client.engines.answer({ botId, id: request.id, ...answer }),
    );
  const selected = pending ? answers : (record.answer?.answers ?? {});
  const summary = [
    record.answer?.text,
    request.options?.find((option) => option.id === record.answer?.optionId)
      ?.label,
  ]
    .filter(Boolean)
    .join(" · ");
  const status =
    record.state === "expired"
      ? isPt
        ? "Expirado"
        : "已失效"
      : record.state === "denied"
        ? question
          ? isPt
            ? "Cancelado"
            : "已取消"
          : isPt
            ? "Rejeitado"
            : "已拒绝"
        : question
          ? isPt
            ? "Respondido"
            : "已回答"
          : isPt
            ? "Aprovado"
            : "已批准";
  const body = (
    <div className="interaction-card" data-state={record.state}>
      <header className="interaction-heading">
        {question ? (
          <QuestionIcon aria-hidden />
        ) : (
          <ShieldCheckIcon aria-hidden />
        )}
        <strong>{request.title}</strong>
      </header>
      {request.fields?.length ? (
        <dl className="approval-fields">
          {request.fields.map((field) => (
            <div key={field.label}>
              <dt>{field.label}</dt>
              <dd>{field.value}</dd>
            </div>
          ))}
        </dl>
      ) : null}
      {request.detail &&
        (pending || request.fields?.length ? (
          <p className="approval-detail">{request.detail}</p>
        ) : (
          <details className="approval-technical">
            <summary>{isPt ? "Conteúdo original" : "原始内容"}</summary>
            <p>{request.detail}</p>
          </details>
        ))}
      {request.url && (
        <a href={request.url} target="_blank" rel="noopener noreferrer">
          {isPt ? "Abrir página de confirmação" : "打开确认页面"}
        </a>
      )}
      {request.questions?.map((item) => (
        <div className="question-field" key={item.id}>
          {item.title !== request.title && <strong>{item.title}</strong>}
          {!!item.options?.length && (
            <div className="question-options">
              {item.options.map((option, index) => (
                <button
                  type="button"
                  key={option.id}
                  className="question-option"
                  disabled={!pending || action.busy}
                  aria-pressed={selected[item.id]?.includes(option.id) ?? false}
                  onClick={() => {
                    const values = item.multiple
                      ? selected[item.id]?.includes(option.id)
                        ? (selected[item.id] ?? []).filter(
                            (id) => id !== option.id,
                          )
                        : [...(selected[item.id] ?? []), option.id]
                      : [option.id];
                    const next = { ...answers, [item.id]: values };
                    setAnswers(next);
                    setCustom(undefined);
                    if (single) void respond({ allow: true, answers: next });
                  }}
                >
                  <span className="option-letter">
                    {String.fromCharCode(65 + index)}
                  </span>
                  <span>
                    {option.label}
                    {option.description && <small>{option.description}</small>}
                  </span>
                </button>
              ))}
            </div>
          )}
          {pending &&
            item.allowOther &&
            !!item.options?.length &&
            custom !== item.id && (
              <Button
                variant="ghost"
                size="small"
                onClick={() => {
                  setCustom(item.id);
                  if (!item.multiple)
                    setAnswers((current) => ({ ...current, [item.id]: [] }));
                }}
              >
                {isPt ? "Resposta personalizada" : "自定义回答"}
              </Button>
            )}
          {pending && (!item.options?.length || custom === item.id) && (
            <TextField
              label={isPt ? "Sua resposta" : "你的回答"}
              multiline={item.multiline}
              value={
                answers[item.id]
                  ?.filter(
                    (value) =>
                      !item.options?.some((option) => option.id === value),
                  )
                  .join(", ") ?? ""
              }
              disabled={action.busy}
              onValueChange={(value) =>
                setAnswers((current) => ({
                  ...current,
                  [item.id]: [
                    ...(item.multiple
                      ? (current[item.id] ?? []).filter((id) =>
                          item.options?.some((option) => option.id === id),
                        )
                      : []),
                    ...(value ? [value] : []),
                  ],
                }))
              }
            />
          )}
          {!pending && !!selected[item.id]?.length && (
            <p className="interaction-result">
              {(selected[item.id] ?? [])
                .map(
                  (value) =>
                    item.options?.find((option) => option.id === value)
                      ?.label ?? value,
                )
                .join("、")}
            </p>
          )}
        </div>
      ))}
      {pending &&
        question &&
        !request.questions?.length &&
        !request.options?.length && (
          <TextField
            label={isPt ? "Sua resposta" : "你的回答"}
            value={text}
            onValueChange={setText}
            disabled={action.busy}
          />
        )}
      {pending ? (
        <div className="interaction-actions">
          <Button
            size="small"
            variant="secondary"
            disabled={action.busy}
            onClick={() => void respond({ allow: false })}
          >
            {question
              ? isPt
                ? "Cancelar"
                : "取消"
              : isPt
                ? "Rejeitar"
                : "拒绝"}
          </Button>
          {request.options?.map((option) => (
            <Button
              key={option.id}
              size="small"
              variant="secondary"
              disabled={action.busy}
              onClick={() => void respond({ allow: true, optionId: option.id })}
            >
              {option.label}
            </Button>
          ))}
          {!request.options?.length &&
            (!single || custom || !request.questions?.[0]?.options?.length) && (
              <Button
                size="small"
                variant="primary"
                disabled={
                  action.busy ||
                  (question &&
                    (request.questions?.length
                      ? request.questions.some(
                          (item) =>
                            item.required !== false &&
                            !answers[item.id]?.some((value) => value.trim()),
                        )
                      : !text.trim()))
                }
                onClick={() =>
                  void respond({
                    allow: true,
                    answers,
                    ...(text ? { text } : {}),
                  })
                }
              >
                {question
                  ? isPt
                    ? "Enviar resposta"
                    : "发送回答"
                  : (request.confirmLabel ?? (isPt ? "Aprovar" : "批准"))}
              </Button>
            )}
        </div>
      ) : summary ? (
        <p className="interaction-result">{summary}</p>
      ) : null}
      {pending && action.error && (
        <p className="alert" role="alert">
          {action.error}
        </p>
      )}
    </div>
  );
  return (
    <section
      aria-label={request.title}
      data-state={record.state}
      className={pending ? undefined : "approval-record"}
    >
      {pending ? (
        body
      ) : (
        <details>
          <summary>
            <InlineNotice
              align="center"
              icon={question ? <QuestionIcon /> : <ShieldCheckIcon />}
            >
              {request.title} · <span role="status">{status}</span>
            </InlineNotice>
          </summary>
          {body}
        </details>
      )}
    </section>
  );
}
