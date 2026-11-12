import type { ComputerView } from "@persona/agent/computer/catalog";
import { Button, Switch } from "@persona/ui/controls";
import { SettingRow } from "@persona/ui/setting-row";
import { client } from "../../client.ts";
import { useI18n } from "../i18n/context.tsx";
import { computerLabels } from "./copy.ts";
import { ComputerDetails, ConfirmComputerAction } from "./dialogs.tsx";

type RecordProps = {
  item: ComputerView;
  admin: boolean;
  busy: boolean;
  error: string;
  defaultId: string | null;
  run: (action: () => Promise<unknown>) => Promise<boolean>;
};

export function ComputerRecord(props: RecordProps) {
  const { item, admin, busy, defaultId, run } = props;
  const { t } = useI18n();
  return (
    <section className="settings-card" aria-label={item.name}>
      <SettingRow
        title={item.name}
        description={`${computerLabels.boundary[item.boundary]} · ${computerLabels.state[item.state]} · ${item.authorized ? t("已授权") : t("待授权")}`}
      >
        <ComputerDetails name={item.name}>
          <dl className="computer-facts">
            <dt>{t("工作根目录")}</dt>
            <dd>{item.directory}</dd>
            <dt>{t("能力")}</dt>
            <dd>
              {item.capabilities.commands ? t("命令与文件") : t("当前不可执行")}
            </dd>
            <dt>{t("网络")}</dt>
            <dd>{computerLabels.network[item.network]}</dd>
          </dl>
        </ComputerDetails>
      </SettingRow>
      {item.error && (
        <p className="alert" role="alert">
          {item.error}
        </p>
      )}
      {admin && <ComputerAccess {...props} />}
      {admin && item.authorized && item.scope?.kind !== "dedicated" && (
        <SettingRow
          title={t("默认电脑")}
          description={t("新建 Bot 时自动分配。已有 Bot 的归属保持不变。")}
        >
          <Switch
            label={`${t("新 Bot 默认使用")}${item.name}`}
            checked={defaultId === item.id}
            disabled={busy}
            onCheckedChange={(checked) =>
              void run(() =>
                client.computer.default({ id: checked ? item.id : null }),
              )
            }
          />
        </SettingRow>
      )}
      {admin && item.authorized && item.kind !== "process" && (
        <ContainerSettings {...props} />
      )}
    </section>
  );
}

function ComputerAccess({ item, busy, error, run }: RecordProps) {
  const { t } = useI18n();
  return (
    <SettingRow
      title={t("工具访问")}
      description={
        item.kind === "process"
          ? t("允许 Bot 使用本机命令与文件。工具使用服务进程的系统权限。")
          : t("允许已分配的 Bot 使用此电脑的命令与文件。")
      }
    >
      {item.authorized ? (
        <Button
          variant="secondary"
          size="small"
          disabled={busy}
          onClick={() =>
            void run(() =>
              client.computer.authorize({ id: item.id, authorized: false }),
            )
          }
        >
          {t("撤销授权")}
        </Button>
      ) : (
        <ConfirmComputerAction
          title={`${t("授权使用")}${item.name}`}
          description={t(
            "命令和文件工具使用服务进程的系统权限。工作目录不提供安全隔离。仅在信任 Bot 的任务时授权。",
          )}
          label={t("确认授权")}
          variant="primary"
          busy={busy}
          error={error}
          onConfirm={() =>
            run(() =>
              client.computer.authorize({ id: item.id, authorized: true }),
            )
          }
        />
      )}
    </SettingRow>
  );
}

function ContainerSettings({ item, busy, error, run }: RecordProps) {
  const { t } = useI18n();
  const running = item.state === "running";
  return (
    <>
      <SettingRow
        title={t("容器运行")}
        description={t(
          "停止容器内的进程。下次使用工具时，自动准备的电脑会重新启动。",
        )}
      >
        <Button
          variant="secondary"
          size="small"
          disabled={busy}
          onClick={() =>
            void run(() =>
              client.computer.manage({
                id: item.id,
                action: running ? "stop" : "start",
              }),
            )
          }
        >
          {running ? t("停止电脑") : t("启动电脑")}
        </Button>
      </SettingRow>
      <SettingRow
        title={t("重建容器")}
        description={t("保留工作卷，并结束容器内的进程。")}
      >
        <ConfirmComputerAction
          title={t("重建工作容器")}
          label={t("确认重建")}
          description={t("工作卷保留。容器内的进程将结束。")}
          busy={busy}
          error={error}
          onConfirm={() =>
            run(() =>
              client.computer.manage({ id: item.id, action: "rebuild" }),
            )
          }
        />
      </SettingRow>
    </>
  );
}
