import type { Bot } from "@persona/agent/bots";
import type { ChatView } from "@persona/agent/view";
import { PanelHeader } from "@persona/ui/panel";
import { ScrollArea } from "@persona/ui/scroll-area";
import { Tabs } from "@persona/ui/tabs";
import { type ReactNode, useState } from "react";
import type { useChat } from "../../chat/use-chat.ts";
import { WorkPanel } from "../../chat/work.tsx";
import type { useComputer } from "../../computer/use-computer.ts";
import { ComputerWorkspace } from "../../computer/workspace.tsx";
import { useI18n } from "../../i18n/context.tsx";
import { ComputerSettings } from "./computer.tsx";
import { BotIdentity, IdentityEditor } from "./identity.tsx";
import { MemoryDialog } from "./memory.tsx";
import { ModelSetup } from "./model.tsx";
import { PromptPanel } from "./prompt.tsx";
import { RootPanel } from "./root.tsx";
import { RoutinesPanel } from "./routines.tsx";
import { SkillsPanel } from "./skills.tsx";
import type { BotPanelView, PanelEditor } from "./types.ts";
import { useBotWork, useWorkAction } from "./use-bot-work.ts";
import "./style.css";

import { translate } from "../../i18n/context.tsx";

export type { BotPanelView } from "./types.ts";

const rawTitles: Record<Exclude<BotPanelView, "root">, string> = {
  identity: "编辑资料",
  engine: "模型与账号",
  advanced: "高级设置",
  computer: "工作电脑",
  skills: "技能",
  routines: "定时",
  prompt: "提示词",
};

const titles = new Proxy(rawTitles, {
  get(_target, prop: Exclude<BotPanelView, "root">) {
    return translate(rawTitles[prop]);
  },
});

export function BotDetails({
  bot,
  active,
  view,
  onViewChange,
  onClose,
  computer,
  chat,
  context,
  canManage,
  tab,
  onTabChange,
  expanded,
  onExpand,
  takeoverId,
  workRun,
}: {
  bot: Bot;
  active: boolean;
  view: BotPanelView;
  onViewChange: (view: BotPanelView) => void;
  onClose: () => void;
  computer: ReturnType<typeof useComputer>;
  chat: ReturnType<typeof useChat> & {
    model: ChatView["model"];
    busy: boolean;
  };
  context: ReactNode;
  canManage: boolean;
  tab: "bot" | "computer" | "browser" | "work";
  onTabChange: (tab: "bot" | "computer" | "browser" | "work") => void;
  expanded: boolean;
  onExpand: () => void;
  takeoverId?: string;
  workRun?: string;
}) {
  const { language } = useI18n();
  const [memoryOpen, setMemoryOpen] = useState(false);
  const workActive =
    active &&
    ((view === "root" && tab === "bot") ||
      view === "skills" ||
      view === "routines" ||
      memoryOpen);
  const work = useBotWork(
    bot.id,
    workActive,
    view !== "root" || memoryOpen,
    active && view === "identity",
  );
  const action = useWorkAction();
  const error =
    action.error ??
    (view === "identity" ? work.profileError : workActive ? work.error : null);
  const [selection, setSelection] = useState<PanelEditor | null>(null);
  const editor = selection?.view === view ? selection : null;
  const navigate = (next: BotPanelView) => {
    setSelection(null);
    onViewChange(next);
  };
  const title = view === "root" ? bot.name : titles[view];
  let content: ReactNode = null;
  if (view === "root" && active)
    content = (
      <RootPanel
        bot={bot}
        data={work.data}
        chat={chat}
        context={context}
        computer={computer}
        canManage={canManage}
        onNavigate={navigate}
        onOpenMemory={() => setMemoryOpen(true)}
      />
    );
  else if (view === "engine") content = <ModelSetup botId={bot.id} />;
  else if (view === "computer")
    content = <ComputerSettings computer={computer} botId={bot.id} />;
  else if (view === "skills")
    content = work.data ? (
      <SkillsPanel
        botId={bot.id}
        items={work.data.skills}
        nativeSkills={work.data.nativeSkills}
        action={action}
        editor={editor?.view === "skills" ? editor : null}
        onEditorChange={setSelection}
      />
    ) : (
      !work.error && <p role="status">{translate("正在读取技能…")}</p>
    );
  else if (view === "routines")
    content = work.data ? (
      <RoutinesPanel
        botId={bot.id}
        data={work.data.routines}
        chat={chat}
        skills={work.data.skills}
        action={action}
        editor={editor?.view === "routines" ? editor : null}
        onEditorChange={setSelection}
      />
    ) : (
      !work.error && <p role="status">{translate("正在读取定时任务…")}</p>
    );
  else if (view === "prompt")
    content = <PromptPanel botId={bot.id} action={action} />;
  return (
    <aside
      tabIndex={-1}
      id="bot-details"
      className="bot-details"
      data-computer={(view === "root" && tab !== "bot") || undefined}
      aria-label={
        language === "zh-CN"
          ? `${bot.name}的详情`
          : `${translate("Detalhes de")} ${bot.name}`
      }
    >
      <PanelHeader
        title={title}
        onBack={
          view === "root"
            ? undefined
            : () => (editor ? setSelection(null) : navigate("root"))
        }
        onClose={onClose}
      />
      {view === "root" && (
        <div className="detail-identity">
          <BotIdentity
            bot={bot}
            profile={work.profile}
            onEdit={() => navigate("identity")}
          />
          <Tabs.Root
            className="bot-tabs"
            value={tab}
            onValueChange={(value) =>
              onTabChange(value as "bot" | "computer" | "browser" | "work")
            }
          >
            <Tabs.List
              className="workspace-tabs"
              aria-label={translate("Bot详情")}
            >
              <Tabs.Tab className="navigation-tab" value="bot">
                Bot
              </Tabs.Tab>
              <Tabs.Tab className="navigation-tab" value="computer">
                Computer
              </Tabs.Tab>
              <Tabs.Tab className="navigation-tab" value="browser">
                Browser
              </Tabs.Tab>
              <Tabs.Tab className="navigation-tab" value="work">
                Work
              </Tabs.Tab>
            </Tabs.List>
          </Tabs.Root>
        </div>
      )}
      {view === "root" && tab === "work" && active && (
        <WorkPanel {...chat} botId={bot.id} selectedRun={workRun} />
      )}
      {view === "root" &&
        (tab === "computer" || tab === "browser") &&
        active && (
          <ComputerWorkspace
            takeoverId={takeoverId}
            key={tab}
            bot={bot}
            computer={computer}
            admin={canManage}
            service={tab === "browser" ? "browser" : "desktop"}
            expanded={expanded}
            onExpand={onExpand}
            onSettings={() => navigate("computer")}
          />
        )}
      <ScrollArea
        hidden={view === "root" && tab !== "bot"}
        className="detail-scroll"
        viewportClassName="detail-content"
      >
        {error && (
          <p className="alert" role="alert">
            {error}
          </p>
        )}
        {view === "identity" && !work.profile && !work.profileError && (
          <p role="status">{translate("正在读取资料…")}</p>
        )}
        {work.profile && (
          <div hidden={view !== "identity"}>
            <IdentityEditor
              key={work.profile.revision}
              profile={work.profile}
              action={action}
              onSaved={work.setProfile}
            />
          </div>
        )}
        {view !== "identity" && (view !== "root" || tab === "bot") && (
          <div
            className="detail-view"
            key={`${view}:${editor ? "editor" : "list"}`}
          >
            {content}
          </div>
        )}
      </ScrollArea>
      <MemoryDialog
        open={memoryOpen}
        onOpenChange={setMemoryOpen}
        botId={bot.id}
        items={work.data?.memory ?? []}
        reviewEnabled={work.data?.memoryReviewEnabled ?? true}
        usage={work.data?.memoryUsage ?? { profile: 0, work: 0 }}
        action={action}
      />
    </aside>
  );
}
