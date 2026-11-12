import { Popover } from "@base-ui/react/popover";
import type { ReactElement, ReactNode } from "react";
import "./context-window.css";

function t(zh: string, pt: string) {
  if (
    typeof document !== "undefined" &&
    document.documentElement.lang === "zh-CN"
  ) {
    return zh;
  }
  return pt;
}

const tokens = (value: number | null | undefined) =>
  value == null
    ? t("未提供", "Não informado")
    : new Intl.NumberFormat("en", {
        notation: "compact",
        maximumFractionDigits: 1,
      }).format(value);

export function contextPercent(
  used: number | null | undefined,
  capacity: number | null | undefined,
) {
  return used != null && capacity != null && capacity > 0
    ? Math.min(100, (used / capacity) * 100)
    : null;
}

const share = (value: number | null) =>
  value == null
    ? "—"
    : `${new Intl.NumberFormat("zh", { maximumFractionDigits: 1 }).format(value)}%`;

export function ContextWindow({
  used,
  capacity,
  estimated,
  trigger,
  children,
}: {
  used: number | null | undefined;
  capacity: number | null | undefined;
  estimated?: boolean;
  trigger: ReactElement;
  children?: ReactNode;
}) {
  const percent = contextPercent(used, capacity);
  const free =
    used != null && capacity != null ? Math.max(0, capacity - used) : null;
  return (
    <Popover.Root>
      <Popover.Trigger render={trigger} />
      <Popover.Portal>
        <Popover.Positioner
          side="bottom"
          align="end"
          sideOffset={10}
          collisionPadding={12}
          className="context-window-positioner"
        >
          <Popover.Popup className="context-window-popup">
            <header className="context-window-head">
              <Popover.Title>
                {t("上下文窗口", "Janela de contexto")}
              </Popover.Title>
              <span
                className="context-window-total"
                title={`${tokens(used)} / ${tokens(capacity)}`}
              >
                {tokens(used)} / {tokens(capacity)}
              </span>
            </header>
            <div
              className="context-window-meter"
              role="img"
              aria-label={`${t("已用", "Usado")} ${share(percent)}`}
            >
              <span
                className="context-window-meter-used"
                style={{ width: `${percent ?? 0}%` }}
              />
            </div>
            <dl className="context-window-rows">
              <div>
                <dt>
                  <i aria-hidden="true" />
                  {t("已用", "Usado")}
                  {estimated ? ` (${t("估算", "estimado")})` : ""}
                </dt>
                <dd>{tokens(used)}</dd>
                <dd>{share(percent)}</dd>
              </div>
              <div>
                <dt>
                  <i aria-hidden="true" className="context-window-free" />
                  {t("剩余", "Restante")}
                </dt>
                <dd>{tokens(free)}</dd>
                <dd>{share(percent == null ? null : 100 - percent)}</dd>
              </div>
            </dl>
            {children}
          </Popover.Popup>
        </Popover.Positioner>
      </Popover.Portal>
    </Popover.Root>
  );
}
