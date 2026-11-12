import { Button } from "@persona/ui/controls";
import { Dialog } from "@persona/ui/dialog";
import { TextField } from "@persona/ui/field";
import { ChoicePicker } from "@persona/ui/picker";
import { PlusIcon } from "@phosphor-icons/react/dist/csr/Plus";
import { useState } from "react";
import { client } from "../../client.ts";
import { useI18n } from "../i18n/context.tsx";
import { userError } from "../user-error.ts";

export function AddComputer({ onAdded }: { onAdded: () => Promise<void> }) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState(() => t("工作电脑"));
  const [id, setId] = useState(() => `work-${crypto.randomUUID().slice(0, 8)}`);
  const [image, setImage] = useState("persona-workspace:3");
  const [host, setHost] = useState("");
  const [user, setUser] = useState("");
  const [knownHostsFile, setKnownHostsFile] = useState("");
  const [identityFile, setIdentityFile] = useState("");
  const [remote, setRemote] = useState(false);
  const [network, setNetwork] = useState<"none" | "web" | "restricted">("none");
  const [proxyImage, setProxyImage] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const desktop = window.personaDesktop;
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!busy) setOpen(next);
      }}
      title={t("添加 Docker 电脑")}
      description={t("连接已准备好工作环境的 Docker 主机。")}
      trigger={
        <Button variant="secondary" size="small">
          <PlusIcon size={14} />
          {t("添加 Docker 电脑")}
        </Button>
      }
      footer={
        <div className="dialog-actions">
          <Button
            type="button"
            variant="ghost"
            disabled={busy}
            onClick={() => setOpen(false)}
          >
            {t("取消")}
          </Button>
          <Button type="submit" form="add-computer" pending={busy}>
            {busy ? t("正在添加…") : t("添加电脑")}
          </Button>
        </div>
      }
    >
      <form
        id="add-computer"
        className="setup-form"
        aria-busy={busy}
        onSubmit={async (event) => {
          event.preventDefault();
          setBusy(true);
          setError("");
          try {
            await client.computer.add({
              name,
              config: {
                kind: "docker",
                id,
                image,
                volume: `persona-${id}-work`,
                network,
                ...(network === "web" ? { proxyImage } : {}),
                cpus: 2,
                memoryMiB: 1024,
                pids: 256,
                timeoutSeconds: 120,
                ...(remote
                  ? {
                      ssh: {
                        host,
                        knownHostsFile,
                        hostKeyAlias: id,
                        ...(user ? { user } : {}),
                        ...(identityFile ? { identityFile } : {}),
                      },
                    }
                  : {}),
              },
            });
            await onAdded();
            setOpen(false);
            setId(`work-${crypto.randomUUID().slice(0, 8)}`);
          } catch (cause) {
            setError(
              cause instanceof Error ? cause.message : t("电脑添加未确认。"),
            );
          } finally {
            setBusy(false);
          }
        }}
      >
        <p className="detail-note">
          {t(
            "使用已准备 Pi 工作镜像的 Docker 主机。远程电脑通过 SSH 接入。选择网络策略后，Bot 按该策略访问网站。",
          )}
        </p>
        {!remote && desktop && (
          <Button
            type="button"
            variant="secondary"
            disabled={busy}
            onClick={async () => {
              setBusy(true);
              setError("");
              try {
                const result = await desktop.prepare();
                if (!result.ok || !result.value) throw new Error(result.error);
                setImage(result.value.image);
                if (result.value.proxyImage) {
                  setProxyImage(result.value.proxyImage);
                  setNetwork("web");
                }
              } catch (cause) {
                setError(userError(cause, t("Docker 准备未完成。")));
              } finally {
                setBusy(false);
              }
            }}
          >
            {t("准备本地 Docker 镜像")}
          </Button>
        )}
        <fieldset className="setup-form" disabled={busy}>
          <TextField
            label={t("电脑名称")}
            value={name}
            maxLength={80}
            required
            onValueChange={setName}
          />
          <TextField
            label={t("电脑标识")}
            value={id}
            pattern="[a-z0-9][a-z0-9_.-]{0,62}"
            required
            onValueChange={setId}
          />
          <TextField
            label={t("工作镜像")}
            value={image}
            required
            onValueChange={setImage}
          />
          <div className="field">
            <span className="field-label">{t("网络策略")}</span>
            <ChoicePicker
              label={t("网络策略")}
              value={network}
              onValueChange={(value) => setNetwork(value as typeof network)}
              items={[
                { value: "none", label: t("关闭外部网络") },
                { value: "web", label: t("允许 HTTP(S) 网站") },
                { value: "restricted", label: t("受限公网出网") },
              ]}
            />
          </div>
          {network === "web" && (
            <TextField
              label={t("代理镜像")}
              value={proxyImage}
              required
              onValueChange={setProxyImage}
            />
          )}
          {network === "restricted" && (
            <p className="detail-note">
              {t("Docker 主机需要安装 Persona 出网防火墙规则。")}
            </p>
          )}
          <div className="field">
            <span className="field-label">{t("Docker 位置")}</span>
            <ChoicePicker
              label={t("Docker 位置")}
              value={remote ? "remote" : "local"}
              onValueChange={(value) => setRemote(value === "remote")}
              items={[
                { value: "local", label: t("本地 Docker") },
                { value: "remote", label: t("远程 Docker") },
              ]}
            />
          </div>
          {remote && (
            <>
              <TextField
                label={t("SSH 主机")}
                value={host}
                required
                onValueChange={setHost}
              />
              <TextField
                label={t("SSH 用户")}
                value={user}
                onValueChange={setUser}
              />
              <TextField
                label={t("主机密钥文件路径")}
                value={knownHostsFile}
                required
                onValueChange={setKnownHostsFile}
              />
              <TextField
                label={t("身份文件路径")}
                value={identityFile}
                onValueChange={setIdentityFile}
              />
            </>
          )}
        </fieldset>
        {error && <p role="alert">{error}</p>}
      </form>
    </Dialog>
  );
}
