import { Button } from "@persona/ui/controls";
import { Dialog } from "@persona/ui/dialog";
import { type ReactNode, useState } from "react";
import { useI18n } from "../i18n/context.tsx";

export function ComputerDetails({
  name,
  children,
}: {
  name: string;
  children: ReactNode;
}) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  return (
    <Dialog
      open={open}
      onOpenChange={setOpen}
      title={name}
      description={t("查看电脑的工作目录、能力与网络。")}
      trigger={
        <Button variant="secondary" size="small">
          {t("查看详情")}
        </Button>
      }
      footer={
        <div className="dialog-actions">
          <Button variant="secondary" onClick={() => setOpen(false)}>
            {t("关闭详情")}
          </Button>
        </div>
      }
    >
      {children}
    </Dialog>
  );
}

export function ConfirmComputerAction({
  title,
  description,
  label,
  busy,
  error,
  onConfirm,
  variant = "secondary",
}: {
  title: string;
  description: string;
  label: string;
  busy: boolean;
  error: string;
  onConfirm: () => Promise<boolean>;
  variant?: "primary" | "secondary";
}) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!busy) setOpen(next);
      }}
      title={title}
      description={description}
      className="decision-dialog"
      trigger={
        <Button variant={variant} size="small" disabled={busy}>
          {title}
        </Button>
      }
      footer={
        <div className="dialog-actions">
          <Button
            variant="ghost"
            disabled={busy}
            onClick={() => setOpen(false)}
          >
            {t("取消")}
          </Button>
          <Button
            disabled={busy}
            onClick={async () => {
              if (await onConfirm()) setOpen(false);
            }}
          >
            {busy ? t("正在处理…") : label}
          </Button>
        </div>
      }
    >
      {error && (
        <p className="alert" role="alert">
          {error}
        </p>
      )}
    </Dialog>
  );
}
