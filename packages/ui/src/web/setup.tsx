import { type ReactNode, useEffect, useRef } from "react";

function t(zh: string, pt: string) {
  if (
    typeof document !== "undefined" &&
    document.documentElement.lang === "zh-CN"
  ) {
    return zh;
  }
  return pt;
}

export function SetupPage({
  title,
  label,
  description,
  progress,
  children,
}: {
  title: string;
  label?: string;
  description?: string;
  progress?: { current: number; total: number };
  children: ReactNode;
}) {
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    if (title) heading.current?.focus({ preventScroll: true });
  }, [title]);
  return (
    <main className="setup-page" aria-label={label ?? title}>
      <div className="setup-content">
        <header className="setup-heading">
          {progress && (
            <div className="setup-progress">
              <span className="setup-dots" aria-hidden="true">
                {Array.from(
                  { length: progress.total },
                  (_, index) => index + 1,
                ).map((step) => (
                  <span
                    key={step}
                    data-complete={step <= progress.current || undefined}
                  />
                ))}
              </span>
              <span role="status">
                {t(
                  `第 ${progress.current} 步，共 ${progress.total} 步`,
                  `Passo ${progress.current} de ${progress.total}`,
                )}
              </span>
            </div>
          )}
          <h1 ref={heading} tabIndex={-1}>
            {title}
          </h1>
          {description && <p>{description}</p>}
        </header>
        {children}
      </div>
    </main>
  );
}
