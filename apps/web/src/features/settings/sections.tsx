import { CpuIcon } from "@phosphor-icons/react/dist/csr/Cpu";
import { DesktopIcon } from "@phosphor-icons/react/dist/csr/Desktop";
import { GearIcon } from "@phosphor-icons/react/dist/csr/Gear";
import type { ReactNode } from "react";
import { ComputerSettings } from "../computer/settings.tsx";
import { translate } from "../i18n/context.tsx";
import { GeneralSettings } from "./general.tsx";
import { ModelSettings } from "./models.tsx";
import { ProviderSettings } from "./providers.tsx";

export type SettingsAccess = {
  admin: boolean;
  deviceId?: string;
  standalone: boolean;
};
type SectionProps = SettingsAccess & {
  botId?: number;
};

export const sections = {
  general: {
    primary: true,
    get label() {
      return translate("通用");
    },
    get description() {
      return translate("主题、设备访问与数据管理。");
    },
    icon: GearIcon,
    render: (props: SectionProps) => <GeneralSettings {...props} />,
  },
  providers: {
    primary: true,
    get label() {
      return translate("Providers");
    },
    get description() {
      return translate("管理 Harness 和模型提供方的连接。");
    },
    icon: CpuIcon,
    render: ({ admin, botId }: SectionProps) =>
      admin ? (
        <ProviderSettings botId={botId} />
      ) : (
        <p className="detail-note">{translate("请让管理员连接模型账号。")}</p>
      ),
  },
  models: {
    primary: true,
    get label() {
      return translate("Models");
    },
    get description() {
      return translate("管理可用模型在聊天选择器中的可见性。");
    },
    icon: CpuIcon,
    render: ({ admin, botId }: SectionProps) => (
      <ModelSettings botId={botId} admin={admin} />
    ),
  },
  computers: {
    primary: false,
    get label() {
      return translate("电脑高级管理");
    },
    get description() {
      return translate("管理镜像、SSH、工作路径和容器生命周期。");
    },
    icon: DesktopIcon,
    render: ({ admin }: SectionProps) => <ComputerSettings admin={admin} />,
  },
} satisfies Record<
  string,
  {
    primary: boolean;
    label: string;
    description: string;
    icon: typeof GearIcon;
    render: (props: SectionProps) => ReactNode;
  }
>;

export type SettingsSection = keyof typeof sections;
export const defaultSection: SettingsSection = "general";
