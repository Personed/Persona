import { Dialog as BaseDialog } from "@base-ui/react/dialog";
import {
  type ReactElement,
  type ReactNode,
  type RefObject,
  useRef,
} from "react";
import { Button } from "./controls.tsx";
import { ScrollArea } from "./scroll-area.tsx";

function t(zh: string, pt: string) {
  if (
    typeof document !== "undefined" &&
    document.documentElement.lang === "zh-CN"
  ) {
    return zh;
  }
  return pt;
}

export { BaseDialog as PanelDialog };

export function Dialog({
  open,
  onOpenChange,
  trigger,
  title,
  description,
  children,
  className,
  finalFocus,
  footer,
  viewportRef,
  headerActions,
  subheader,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  trigger?: ReactElement;
  title: ReactNode;
  description?: string;
  children: ReactNode;
  className?: string;
  finalFocus?: () => HTMLElement | null;
  footer?: ReactNode;
  viewportRef?: RefObject<HTMLDivElement | null>;
  headerActions?: ReactNode;
  subheader?: ReactNode;
}) {
  const internalBody = useRef<HTMLDivElement>(null);
  const body = viewportRef ?? internalBody;
  return (
    <BaseDialog.Root open={open} onOpenChange={onOpenChange}>
      {trigger && <BaseDialog.Trigger render={trigger} />}
      <BaseDialog.Portal>
        <BaseDialog.Backdrop className="dialog-backdrop" forceRender />
        <BaseDialog.Popup
          className={`dialog-popup ${className ?? ""}`}
          finalFocus={finalFocus}
          initialFocus={() =>
            Array.from(
              body.current?.querySelectorAll<HTMLElement>(
                "button, input, textarea, a[href], [role=combobox]",
              ) ?? [],
            ).find(
              (element) =>
                element.checkVisibility() && !element.matches(":disabled"),
            ) ??
            body.current?.parentElement?.querySelector<HTMLElement>(
              ".dialog-footer button",
            ) ??
            body.current
          }
        >
          <header className="dialog-header">
            <BaseDialog.Title className="dialog-title">
              {title}
            </BaseDialog.Title>
            {headerActions}
            <BaseDialog.Close
              aria-label={t("关闭弹窗", "Fechar")}
              render={<Button variant="ghost" size="icon-small" />}
            >
              <svg viewBox="0 0 16 16" aria-hidden="true">
                <path d="m4 4 8 8m0-8-8 8" />
              </svg>
            </BaseDialog.Close>
          </header>
          {description && (
            <BaseDialog.Description className="dialog-description">
              {description}
            </BaseDialog.Description>
          )}
          {subheader && <div className="dialog-subheader">{subheader}</div>}
          <ScrollArea className="dialog-body" viewportRef={body}>
            {children}
          </ScrollArea>
          {footer && <footer className="dialog-footer">{footer}</footer>}
        </BaseDialog.Popup>
      </BaseDialog.Portal>
    </BaseDialog.Root>
  );
}

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  onConfirm,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description: string;
  confirmLabel?: string;
  onConfirm: () => void;
}) {
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={title}
      description={description}
      className="decision-dialog"
      footer={
        <div className="dialog-actions">
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            {t("取消", "Cancelar")}
          </Button>
          <Button
            onClick={() => {
              onOpenChange(false);
              onConfirm();
            }}
          >
            {confirmLabel ?? t("删除", "Excluir")}
          </Button>
        </div>
      }
    >
      {null}
    </Dialog>
  );
}
