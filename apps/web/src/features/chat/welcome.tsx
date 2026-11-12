import { Button } from "@persona/ui/controls";
import { ArrowUpRightIcon } from "@phosphor-icons/react/dist/csr/ArrowUpRight";
import { XIcon } from "@phosphor-icons/react/dist/csr/X";
import { useState } from "react";
import { client } from "../../client.ts";
import { useI18n } from "../i18n/context.tsx";
import "./welcome.css";

const suggestionsPtBR = [
  {
    label: "Redigir um documento",
    text: "Ajude-me a redigir um documento. Pergunte-me primeiro sobre o objetivo e os requisitos.",
  },
  {
    label: "Analisar informações",
    text: "Ajude-me a analisar um material ou relatório. Pergunte-me sobre o conteúdo e minhas dúvidas principais.",
  },
  {
    label: "Organizar arquivos",
    text: "Ajude-me a organizar meus arquivos de trabalho. Confirme os diretórios e regras antes de começar.",
  },
];

const suggestionsZhCN = [
  { label: "起草一份文档", text: "帮我起草一份文档。先问我用途和要求。" },
  {
    label: "分析一份资料",
    text: "帮我分析一份资料。先问我资料内容和我关心的问题。",
  },
  {
    label: "整理工作文件",
    text: "帮我整理工作文件。先确认目录和整理规则，再开始操作。",
  },
];

export function ChatWelcome({
  botId,
  disabled,
  onChoose,
  onDismissed,
}: {
  botId: number;
  disabled: boolean;
  onChoose: (text: string) => void;
  onDismissed: () => void;
}) {
  const { language, t } = useI18n();
  const suggestions = language === "zh-CN" ? suggestionsZhCN : suggestionsPtBR;
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  return (
    <section
      className="chat-welcome"
      aria-label={language === "zh-CN" ? "入门提示" : "Dicas iniciais"}
      aria-busy={pending}
    >
      <header>
        <h2>{t("想让我先帮你做什么？")}</h2>
        <Button
          variant="ghost"
          size="icon-small"
          aria-label={language === "zh-CN" ? "关闭入门提示" : "Fechar dicas"}
          disabled={disabled}
          pending={pending}
          onClick={async () => {
            setPending(true);
            setError("");
            try {
              await client.bots.completeWelcome({ botId });
              onDismissed();
            } catch {
              setError(t("关闭入门提示失败，请重试。"));
            } finally {
              setPending(false);
            }
          }}
        >
          {!pending && <XIcon />}
        </Button>
      </header>
      <p>{t("选一个开始，也可以直接告诉我。")}</p>
      <div className="welcome-suggestions">
        {suggestions.map(({ label, text }) => (
          <Button
            key={label}
            variant="secondary"
            disabled={disabled || pending}
            onClick={() => onChoose(text)}
          >
            {label}
            <ArrowUpRightIcon aria-hidden="true" />
          </Button>
        ))}
      </div>
      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}
    </section>
  );
}
