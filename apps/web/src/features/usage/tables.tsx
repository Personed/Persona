import type { EngineId } from "@persona/agent/harness/types";
import { SearchField } from "@persona/ui/field";
import { useState } from "react";
import { EngineMark } from "../bots/engine-mark.tsx";
import { getLanguage } from "../i18n/context.tsx";
import {
  type Dimension,
  formatValue,
  harnesses,
  type Metric,
  metricUnit,
  money,
  recordTime,
  type UsageGroup,
  type UsageRecord,
  usageValue,
} from "./data.ts";

const count = (value: number | null) => {
  const isPt = getLanguage() !== "zh-CN";
  return value === null
    ? isPt
      ? "Não informado"
      : "未提供"
    : value.toLocaleString();
};

const harnessLabel = (id: string) =>
  harnesses.find((item) => item.value === id)?.label ?? id;

export function Breakdown({
  groups,
  metric,
  dimension,
}: {
  groups: UsageGroup[];
  metric: Metric;
  dimension: Dimension;
}) {
  const isPt = getLanguage() !== "zh-CN";
  const total = usageValue(
    groups.flatMap((group) => group.rows),
    metric,
  );
  const max = Math.max(...groups.map((group) => group.value ?? 0), 0);
  return (
    <ul
      className="usage-breakdown"
      aria-label={isPt ? "Uso por grupo" : "分组用量"}
    >
      {groups.map((group) => {
        const share =
          total && group.value !== null ? (group.value / total) * 100 : null;
        return (
          <li key={group.key}>
            <span className="usage-breakdown-label">
              {dimension === "harness" && group.key !== "other" ? (
                <EngineMark provider={group.key as EngineId} />
              ) : (
                <i className="usage-dot" style={{ background: group.color }} />
              )}
              <span className="usage-truncate">{group.label}</span>
            </span>
            <span className="usage-breakdown-value">
              {formatValue(group.value, metric)}
              <small>{share === null ? "—" : `${share.toFixed(1)}%`}</small>
            </span>
            <span className="usage-breakdown-track" aria-hidden="true">
              <span
                style={{
                  width: `${max && group.value !== null ? Math.max(2, (group.value / max) * 100) : 0}%`,
                  background: group.color,
                }}
              />
            </span>
          </li>
        );
      })}
      <li className="usage-breakdown-total">
        <span>{isPt ? "Total" : "合计"}</span>
        <span>
          {metric === "cost" ? money(total) : formatValue(total, metric)}
          <small>{metricUnit(metric)}</small>
        </span>
      </li>
    </ul>
  );
}

export function Records({
  rows,
  names,
}: {
  rows: UsageRecord[];
  names: Map<number, string>;
}) {
  const isPt = getLanguage() !== "zh-CN";
  const [query, setQuery] = useState("");
  const needle = query.trim().toLowerCase();
  const matches = needle
    ? rows.filter(
        (row) =>
          (names.get(row.botId) ?? `Bot ${row.botId}`)
            .toLowerCase()
            .includes(needle) ||
          (row.model ?? "").toLowerCase().includes(needle) ||
          (row.source ?? "").toLowerCase().includes(needle) ||
          (row.provider ?? "").toLowerCase().includes(needle),
      )
    : rows;
  return (
    <>
      <div className="usage-search">
        <SearchField
          aria-label={isPt ? "Buscar registros de uso" : "搜索用量记录"}
          placeholder={
            isPt ? "Buscar Bot, modelo ou origem" : "搜索 Bot、模型或来源"
          }
          spellCheck={false}
          value={query}
          onValueChange={setQuery}
        />
        <span aria-live="polite">
          {needle
            ? `${matches.length} / ${rows.length}`
            : `${rows.length} ${isPt ? "itens" : "条"}`}
        </span>
      </div>
      {matches.length ? (
        <div className="usage-table-scroll">
          <table
            className="usage-table usage-records"
            aria-label={isPt ? "Detalhamento de uso" : "用量明细"}
          >
            <thead>
              <tr>
                <th scope="col">{isPt ? "Horário · UTC" : "时间 · UTC"}</th>
                <th scope="col">Bot</th>
                <th scope="col">Harness</th>
                <th scope="col">{isPt ? "Modelo" : "模型"}</th>
                <th scope="col" className="usage-cell-value">
                  {isPt ? "Entrada" : "输入"}
                </th>
                <th scope="col" className="usage-cell-value">
                  {isPt ? "Saída" : "输出"}
                </th>
                <th scope="col" className="usage-cell-value">
                  {isPt ? "Leitura / Escrita de cache" : "缓存读 / 写"}
                </th>
                <th scope="col" className="usage-cell-value">
                  {isPt ? "Custo" : "成本"}
                </th>
              </tr>
            </thead>
            <tbody>
              {matches.map((row) => (
                <tr
                  key={`${row.taskId}:${row.id}`}
                  title={`${row.taskId} · ${row.sessionId ?? (isPt ? "Sessão não informada" : "会话未提供")} · ${row.source}`}
                >
                  <td className="usage-time">{recordTime(row.timestamp)}</td>
                  <th scope="row">
                    <span className="usage-truncate">
                      {names.get(row.botId) ?? `Bot ${row.botId}`}
                    </span>
                  </th>
                  <td>
                    <span className="usage-harness">
                      <EngineMark provider={row.harness} />
                      <span>
                        {harnessLabel(row.harness)}
                        <small>
                          {row.provider ?? (isPt ? "Não informado" : "未提供")}
                        </small>
                      </span>
                    </span>
                  </td>
                  <td title={row.model ?? (isPt ? "Não informado" : "未提供")}>
                    <span className="usage-truncate usage-mono">
                      {row.model ?? (isPt ? "Não informado" : "未提供")}
                    </span>
                  </td>
                  <td className="usage-cell-value">{count(row.input)}</td>
                  <td className="usage-cell-value">{count(row.output)}</td>
                  <td className="usage-cell-value">
                    {count(row.cacheRead)} / {count(row.cacheWrite)}
                  </td>
                  <td className="usage-cell-value">
                    {money(row.costUsd)}
                    {row.costScope === "session" && (
                      <small>{isPt ? "Acumulado da sessão" : "会话累计"}</small>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="usage-muted" role="status">
          {isPt
            ? "Nenhum registro correspondente encontrado."
            : "没有匹配的记录。"}
        </p>
      )}
    </>
  );
}
