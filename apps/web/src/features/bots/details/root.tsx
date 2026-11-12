import type { Bot } from "@persona/agent/bots";
import type { ChatView } from "@persona/agent/view";
import { SegmentedControl } from "@persona/ui/choice";
import { PanelGroup, PanelRow } from "@persona/ui/panel";
import { SettingRow } from "@persona/ui/setting-row";
import { BrainIcon } from "@phosphor-icons/react/dist/csr/Brain";
import { ClockIcon } from "@phosphor-icons/react/dist/csr/Clock";
import { CpuIcon } from "@phosphor-icons/react/dist/csr/Cpu";
import { LightningIcon } from "@phosphor-icons/react/dist/csr/Lightning";
import { TerminalWindowIcon } from "@phosphor-icons/react/dist/csr/TerminalWindow";
import type { ReactNode } from "react";
import type { useComputer } from "../../computer/use-computer.ts";
import { useI18n } from "../../i18n/context.tsx";
import { ModelSetting } from "../../models/model-setting.tsx";
import type { BotPanelView } from "./types.ts";
import type { WorkData } from "./use-bot-work.ts";

export function RootPanel({
  bot,
  data,
  chat,
  context,
  computer,
  canManage,
  onNavigate,
  onOpenMemory,
}: {
  bot: Bot;
  data: WorkData | null;
  chat: { model: ChatView["model"]; connected: boolean; busy: boolean };
  context: ReactNode;
  computer: ReturnType<typeof useComputer>;
  canManage: boolean;
  onNavigate: (view: BotPanelView) => void;
  onOpenMemory: () => void;
}) {
  const { t, language } = useI18n();
  const isPt = language === "pt-BR";
  const memory = data?.memory.length ?? 0;
  const skills = new Set([
    ...(data?.nativeSkills?.map((item) => item.name) ?? []),
    ...(data?.skills
      .filter((item) => item.enabledBots.includes(bot.id))
      .map((item) => item.name) ?? []),
  ]).size;
  const routines =
    data?.routines.items.filter((item) => item.enabled).length ?? 0;
  return (
    <div className="panel-page">
      {computer.error && (
        <p className="alert" role="alert">
          {computer.error}
        </p>
      )}

      <PanelGroup title={t("运行")}>
        <div className="engine-choices">
          <ModelSetting
            botId={bot.id}
            connected={chat.connected}
            disabled={computer.busy || chat.busy}
          />
          <SettingRow title={t("上下文")}>{context}</SettingRow>
          <SettingRow title={isPt ? "Aprovação de ações" : "操作审批"}>
            <SegmentedControl
              label={isPt ? "Aprovação" : "操作审批"}
              value={computer.approvalMode}
              onValueChange={computer.setApprovalMode}
              disabled={
                !canManage || computer.busy || !computer.status?.authorized
              }
              options={[
                { value: "ask", label: isPt ? "Perguntar" : "询问" },
                {
                  value: "auto",
                  label: isPt ? "Automático" : "自动",
                  title: isPt
                    ? "Aprova ações locais automaticamente. Perguntas e logins ainda exigem confirmação."
                    : "自动批准当前工作电脑上的操作。提问和登录仍需确认。",
                },
              ]}
            />
          </SettingRow>
        </div>
        <PanelRow
          icon={<CpuIcon />}
          title={t("连接与账号")}
          value={isPt ? "Computador de trabalho" : "工作电脑"}
          onClick={() => onNavigate("engine")}
        />
      </PanelGroup>

      <PanelGroup title={t("能力")}>
        <PanelRow
          icon={<BrainIcon />}
          title={t("记忆")}
          value={
            data
              ? isPt
                ? `${memory} itens`
                : `${memory} 条`
              : isPt
                ? "Carregando…"
                : "读取中"
          }
          onClick={onOpenMemory}
        />
        <PanelRow
          icon={<LightningIcon />}
          title={t("技能")}
          value={
            data
              ? isPt
                ? `${skills} ativas`
                : `${skills} 个启用`
              : isPt
                ? "Carregando…"
                : "读取中"
          }
          onClick={() => onNavigate("skills")}
        />
        <PanelRow
          icon={<ClockIcon />}
          title={t("定时")}
          value={
            data
              ? isPt
                ? `${routines} ativas`
                : `${routines} 个已启用`
              : isPt
                ? "Carregando…"
                : "读取中"
          }
          onClick={() => onNavigate("routines")}
        />
        <PanelRow
          icon={<TerminalWindowIcon />}
          title={t("提示词")}
          value={t("只读")}
          onClick={() => onNavigate("prompt")}
        />
      </PanelGroup>
    </div>
  );
}
