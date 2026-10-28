import type { PromptPoint } from "./types";

export interface PromptChartPoint extends PromptPoint {
  x: number;
  y: number;
}

// Keep the raw values for tooltips; Chart.js uses x/y for the axes.
export function preparePromptPoints(points: readonly PromptPoint[] | null | undefined): PromptChartPoint[] {
  if (!Array.isArray(points)) return [];
  return points.flatMap((point) => {
    if (
      !point ||
      typeof point.model !== "string" ||
      !Number.isSafeInteger(point.input_tokens) ||
      point.input_tokens <= 0 ||
      !Number.isFinite(point.prompt_per_second) ||
      point.prompt_per_second <= 0
    ) return [];

    return [{
      ...point,
      x: point.input_tokens,
      y: point.prompt_per_second,
    }];
  });
}

export function promptLogBounds(points: readonly PromptChartPoint[]): { min: number; max: number } {
  let min = Infinity;
  let max = 0;
  for (const point of points) {
    min = Math.min(min, point.x);
    max = Math.max(max, point.x);
  }
  if (!Number.isFinite(min) || max <= 0) return { min: 1, max: 10 };
  const lower = Math.max(1, 10 ** Math.floor(Math.log10(min)));
  const upper = 10 ** Math.ceil(Math.log10(max));
  return { min: lower, max: upper <= lower ? lower * 10 : upper };
}

const compactNumber = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });
const number = new Intl.NumberFormat();
const speed = new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });

export function formatPromptTick(value: number): string {
  // Keep the small graph readable: label powers of ten, rather than every tick.
  if (value <= 0 || !Number.isFinite(value) || Math.abs(Math.log10(value) - Math.round(Math.log10(value))) > 1e-10) return "";
  return compactNumber.format(value);
}

export function promptTooltipLines(point: PromptPoint): string[] {
  const cached = Number.isSafeInteger(point.cache_tokens) && point.cache_tokens >= 0
    ? number.format(point.cache_tokens)
    : "Unknown";
  return [
    `Prompt: ${number.format(point.input_tokens)} tokens`,
    `Cached: ${cached}${cached === "Unknown" ? "" : " tokens"}`,
    `Prefill: ${speed.format(point.prompt_per_second)} t/s`,
  ];
}
