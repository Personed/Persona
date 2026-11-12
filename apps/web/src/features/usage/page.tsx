import { SegmentedControl } from "@persona/ui/choice";
import { Button } from "@persona/ui/controls";
import { ChoicePicker } from "@persona/ui/picker";
import { ScrollArea } from "@persona/ui/scroll-area";
import { ArrowClockwiseIcon } from "@phosphor-icons/react/dist/csr/ArrowClockwise";
import { ChartBarIcon } from "@phosphor-icons/react/dist/csr/ChartBar";
import { useEffect, useMemo, useState } from "react";
import { client } from "../../client.ts";
import { useI18n } from "../i18n/context.tsx";
import { userError } from "../user-error.ts";
import { DailyChart, Legend } from "./charts.tsx";
import {
  compactNumber,
  type Dimension,
  dailyUsage,
  dayLabel,
  getDimensions,
  getMetrics,
  harnesses,
  type Metric,
  metricLabel,
  money,
  type UsagePeriod,
  type UsageRecord,
  usageGroups,
  usagePeriodRange,
  usageTotals,
  usageValue,
} from "./data.ts";
import { Breakdown, Records } from "./tables.tsx";
import "./style.css";

const periodsPt = [
  { value: "month", label: "Este mês" },
  { value: "lastMonth", label: "Mês passado" },
  { value: "days30", label: "Últimos 30 dias" },
];
const periodsZh = [
  { value: "month", label: "本月" },
  { value: "lastMonth", label: "上月" },
  { value: "days30", label: "近 30 天" },
];
const limits = [
  { value: "5", label: "Top 5" },
  { value: "10", label: "Top 10" },
  { value: "20", label: "Top 20" },
];

function Filter({
  label,
  value,
  items,
  onChange,
}: {
  label: string;
  value: string;
  items: { value: string; label: string }[];
  onChange: (value: string) => void;
}) {
  return (
    <span className="usage-chip" data-active={Boolean(value) || undefined}>
      <ChoicePicker
        label={label}
        size="small"
        value={value}
        onValueChange={onChange}
        items={items}
      />
    </span>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="usage-kpi">
      <dt>{label}</dt>
      <dd>{value}</dd>
    </div>
  );
}

export function UsageSurface() {
  const { language } = useI18n();
  const isPt = language !== "zh-CN";
  const periods = isPt ? periodsPt : periodsZh;
  const metrics = getMetrics();
  const dimensions = getDimensions();
  const [period, setPeriod] = useState<UsagePeriod>("month");
  const [harness, setHarness] = useState("");
  const [provider, setProvider] = useState("");
  const [botId, setBotId] = useState("");
  const [model, setModel] = useState("");
  const [metric, setMetric] = useState<Metric>("cost");
  const [dimension, setDimension] = useState<Dimension>("model");
  const [limit, setLimit] = useState("5");
  const [rows, setRows] = useState<UsageRecord[]>([]);
  const [bots, setBots] = useState<{ id: number; name: string }[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);
  const [elapsed, setElapsed] = useState(0);

  const range = useMemo(() => usagePeriodRange(period), [period]);

  useEffect(() => {
    let current = true;
    void client.bots
      .list()
      .then((records) => {
        if (current)
          setBots(records.map((bot) => ({ id: bot.id, name: bot.name })));
      })
      .catch(() => {});
    return () => {
      current = false;
    };
  }, []);

  // biome-ignore lint/correctness/useExhaustiveDependencies: nonce forces a refetch when the user presses refresh.
  useEffect(() => {
    let current = true;
    setLoading(true);
    setError(null);
    const started = performance.now();
    void client.engines
      .usage({
        from: Date.parse(`${range.from}T00:00:00.000Z`),
        to: Date.parse(`${range.to}T23:59:59.999Z`),
      })
      .then((records) => {
        if (!current) return;
        setRows(records);
        setElapsed(Math.round(performance.now() - started));
        setLoading(false);
      })
      .catch((cause) => {
        if (current)
          setError(
            userError(
              cause,
              isPt
                ? "Falha ao carregar uso. Tente novamente."
                : "用量读取失败。请重试。",
            ),
          );
        if (current) setLoading(false);
      });
    return () => {
      current = false;
    };
  }, [range.from, range.to, nonce, isPt]);

  const refresh = () => setNonce((value) => value + 1);

  const names = useMemo(
    () => new Map(bots.map((bot) => [bot.id, bot.name])),
    [bots],
  );

  const visible = useMemo(
    () =>
      rows.filter(
        (row) =>
          (!harness || row.harness === harness) &&
          (!provider || row.provider === provider) &&
          (!botId || row.botId === Number(botId)) &&
          (!model || row.model === model),
      ),
    [rows, harness, provider, botId, model],
  );

  const totals = useMemo(() => usageTotals(visible), [visible]);
  const days = useMemo(
    () => dailyUsage(visible, range.from, range.to),
    [visible, range.from, range.to],
  );
  const groups = useMemo(
    () => usageGroups(visible, dimension, metric, names, Number(limit)),
    [visible, dimension, metric, names, limit],
  );

  const peak = useMemo(() => {
    let topDay = "";
    let topCost: number | null = null;
    for (const day of days) {
      const cost = usageValue(day.rows, "cost");
      if (cost !== null && (topCost === null || cost > topCost)) {
        topCost = cost;
        topDay = day.day;
      }
    }
    return { day: topDay, cost: topCost };
  }, [days]);

  const activeDays = useMemo(
    () => days.filter((day) => day.rows.length > 0).length,
    [days],
  );

  const clearFilters = () => {
    setHarness("");
    setProvider("");
    setBotId("");
    setModel("");
  };
  const activeFilters =
    Number(Boolean(harness)) +
    Number(Boolean(provider)) +
    Number(Boolean(botId)) +
    Number(Boolean(model));

  const options = (values: (string | null)[], all: string) => [
    { value: "", label: all },
    ...[...new Set(values.filter((value): value is string => value !== null))]
      .sort()
      .map((value) => ({ value, label: value })),
  ];
  return (
    <ScrollArea className="usage-surface">
      <main
        className="usage-page"
        aria-label={isPt ? "Uso" : "用量"}
        aria-busy={loading}
      >
        <header className="usage-head">
          <div className="usage-head-text">
            <h1>{isPt ? "Uso" : "用量"}</h1>
            <p>
              {isPt
                ? "Acompanhe o uso e os custos nativos do motor por Provedor, Harness, Bot e Modelo."
                : "按 Provider、Harness、Bot 与模型查看引擎原生用量与成本。"}
            </p>
          </div>
          <div className="usage-head-actions">
            <span className="usage-timezone">UTC</span>
            <ChoicePicker
              label={isPt ? "Intervalo de datas" : "日期范围"}
              size="small"
              value={period}
              onValueChange={(value) => setPeriod(value as UsagePeriod)}
              items={periods}
            />
            <Button
              size="small"
              variant="secondary"
              aria-label={isPt ? "Atualizar uso" : "刷新用量"}
              title={isPt ? "Atualizar uso" : "刷新用量"}
              disabled={loading}
              onClick={refresh}
            >
              <ArrowClockwiseIcon />
            </Button>
          </div>
        </header>
        <fieldset
          className="usage-filters"
          aria-label={isPt ? "Filtrar uso" : "筛选用量"}
        >
          <Filter
            label="Harness"
            value={harness}
            onChange={setHarness}
            items={[{ value: "", label: "Harness" }, ...harnesses]}
          />
          <Filter
            label="Provider"
            value={provider}
            onChange={setProvider}
            items={options(
              rows.map((row) => row.provider),
              "Provider",
            )}
          />
          <Filter
            label="Bot"
            value={botId}
            onChange={setBotId}
            items={[
              { value: "", label: "Bot" },
              ...bots.map((bot) => ({
                value: String(bot.id),
                label: bot.name,
              })),
            ]}
          />
          <Filter
            label={isPt ? "Modelo de uso" : "用量模型"}
            value={model}
            onChange={setModel}
            items={options(
              rows.map((row) => row.model),
              isPt ? "Modelo" : "模型",
            )}
          />
          {activeFilters > 0 && (
            <Button size="small" variant="ghost" onClick={clearFilters}>
              {isPt ? "Limpar" : "清除"}
            </Button>
          )}
        </fieldset>
        {error ? (
          <div className="usage-error" role="alert">
            <p>{error}</p>
            <Button variant="secondary" size="small" onClick={refresh}>
              {isPt ? "Tentar novamente" : "重试"}
            </Button>
          </div>
        ) : loading ? (
          <div
            className="usage-loading"
            role="status"
            aria-label={isPt ? "Carregando uso" : "正在读取用量"}
          >
            <div className="usage-skeleton usage-skeleton-hero" />
            <div className="usage-skeleton-stats" aria-hidden="true">
              {[0, 1, 2, 3, 4].map((slot) => (
                <span className="usage-skeleton" key={slot} />
              ))}
            </div>
            <div className="usage-skeleton usage-skeleton-chart" />
          </div>
        ) : !visible.length ? (
          <div className="usage-empty" role="status">
            <ChartBarIcon size={26} weight="thin" aria-hidden="true" />
            <h2>
              {isPt
                ? "Nenhum registro de uso neste período."
                : "此范围还没有用量记录。"}
            </h2>
            <p>
              {isPt
                ? "Após a conclusão das tarefas, o uso fornecido pelo motor será exibido aqui."
                : "任务完成后，引擎提供的用量会显示在这里。"}
            </p>
          </div>
        ) : (
          <>
            <section
              className="usage-hero"
              aria-label={isPt ? "Visão geral de uso" : "用量总览"}
            >
              <div className="usage-hero-main">
                <span className="usage-hero-kicker">
                  {isPt ? "Custo reportado de tarefas" : "已报告任务成本"}
                </span>
                <strong className="usage-hero-value">
                  {money(totals.cost)}
                </strong>
                <p className="usage-hero-meta">
                  USD · {periods.find((item) => item.value === period)?.label} ·{" "}
                  {totals.tasks} {isPt ? "tarefas" : "个任务"} ·{" "}
                  {totals.records} {isPt ? "registros" : "条记录"}
                </p>
              </div>
              <dl className="usage-facts">
                <div>
                  <dt>{isPt ? "Dia de pico" : "峰值日"}</dt>
                  <dd>
                    {peak.day ? dayLabel(peak.day) : "—"}
                    <small>{money(peak.cost)}</small>
                  </dd>
                </div>
                <div>
                  <dt>
                    {isPt
                      ? `Média diária (${activeDays} dias com registros)`
                      : `日均（有记录 ${activeDays} 天）`}
                  </dt>
                  <dd>
                    <small>
                      {activeDays
                        ? money((totals.cost ?? 0) / activeDays)
                        : "—"}
                    </small>
                  </dd>
                </div>
              </dl>
            </section>
            <dl className="usage-stats">
              <Stat
                label={isPt ? "Tarefas" : "任务数"}
                value={compactNumber(totals.tasks)}
              />
              <Stat
                label={isPt ? "Tokens de entrada" : "输入用量"}
                value={
                  totals.input === null ? "—" : compactNumber(totals.input)
                }
              />
              <Stat
                label={isPt ? "Tokens de saída" : "输出用量"}
                value={
                  totals.output === null ? "—" : compactNumber(totals.output)
                }
              />
              <Stat
                label={isPt ? "Leitura de cache" : "缓存读"}
                value={
                  totals.cacheRead === null
                    ? "—"
                    : compactNumber(totals.cacheRead)
                }
              />
              <Stat
                label={isPt ? "Escrita de cache" : "缓存写"}
                value={
                  totals.cacheWrite === null
                    ? "—"
                    : compactNumber(totals.cacheWrite)
                }
              />
            </dl>
            <section
              className="usage-section"
              aria-label={isPt ? "Tendência diária" : "每日趋势"}
            >
              <header className="usage-section-head">
                <h2>
                  {isPt
                    ? `${metricLabel(metric)} · Tendência`
                    : `${metricLabel(metric)}趋势`}
                </h2>
                <SegmentedControl
                  label={isPt ? "Métrica estatística" : "统计指标"}
                  value={metric}
                  onValueChange={setMetric}
                  options={metrics}
                />
              </header>
              <DailyChart days={days} groups={groups} metric={metric} />
              {groups.length > 1 && <Legend groups={groups} />}
            </section>
            <section
              className="usage-section"
              aria-label={isPt ? "Composição" : "构成"}
            >
              <header className="usage-section-head">
                <h2>{isPt ? "Composição" : "构成"}</h2>
                <div className="usage-section-tools">
                  <SegmentedControl
                    label={isPt ? "Dimensão de agrupamento" : "分组维度"}
                    value={dimension}
                    onValueChange={setDimension}
                    options={dimensions}
                  />
                  <ChoicePicker
                    label={isPt ? "Quantidade exibida" : "显示数量"}
                    size="small"
                    value={limit}
                    onValueChange={setLimit}
                    items={limits}
                  />
                </div>
              </header>
              <Breakdown
                groups={groups}
                metric={metric}
                dimension={dimension}
              />
            </section>
            <section
              className="usage-section"
              aria-label={isPt ? "Detalhamento de uso" : "用量明细"}
            >
              <header className="usage-section-head">
                <h2>{isPt ? "Todos os registros" : "全部记录"}</h2>
                <span className="usage-section-meta">
                  {new Set(visible.map((row) => row.taskId)).size}{" "}
                  {isPt ? "tarefas" : "个任务"} · {visible.length}{" "}
                  {isPt ? "registros" : "条"}
                </span>
              </header>
              <Records rows={visible} names={names} />
            </section>
            <footer className="usage-footer">
              <span>
                {totals.tasks} {isPt ? "tarefas" : "个任务"} · {totals.records}{" "}
                {isPt ? "registros de uso" : "条用量记录"} · {elapsed}
                ms {isPt ? "para carregar" : "读取"}
              </span>
              <span>
                {totals.unpriced}{" "}
                {isPt ? "sem custo informado" : "条成本未提供"} ·{" "}
                {totals.snapshots} {isPt ? "acumulado de sessão" : "条会话累计"}
              </span>
            </footer>
            <p className="usage-note">
              {isPt
                ? "Exibindo apenas dados retornados pelo motor · UTC · USD. O custo da tarefa não inclui snapshots de sessão acumulados; entrada e cache preservam as métricas nativas."
                : "仅显示引擎返回的数据 · UTC · USD。任务成本不含会话累计快照；输入与缓存保留原生口径。"}
            </p>
          </>
        )}
      </main>
    </ScrollArea>
  );
}

export const UsagePage = UsageSurface;
