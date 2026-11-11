import type { DraftPoint, GenerationPoint, PromptPoint } from "./types";
import { preparePromptPoints, promptTooltipLines } from "./promptScatter";

export type ActivityMetric = "prompt" | "generation" | "draft";
export type ChartType = "scatter" | "histogram";

export function normalizeChartType(value: unknown): ChartType {
  return value === "histogram" ? "histogram" : "scatter";
}

export const activityChartSettings = {
  prompt: {
    title: "Prompt Processing",
    tooltip: "Prompt size includes cached tokens. Prefill speed measures processing of uncached tokens.",
    yLabel: "Prefill (t/s)",
    unit: "prompt tokens/sec",
    color: "rgba(245, 158, 11, 0.6)",
    colorClass: "text-amber-500 dark:text-amber-400",
    empty: "No prompt speed data yet",
  },
  generation: {
    title: "Token Generation",
    tooltip: "Generation speed by prompt size. Token counts match the Prompt column.",
    yLabel: "Generation (t/s)",
    unit: "tokens/sec",
    color: "rgba(59, 130, 246, 0.6)",
    colorClass: "text-blue-500 dark:text-blue-400",
    empty: "No generation speed data yet",
  },
  draft: {
    title: "Draft Acceptance",
    tooltip: "Draft acceptance by prompt size: accepted draft tokens divided by proposed draft tokens.",
    yLabel: "Acceptance (%)",
    unit: "acceptance rate (%)",
    color: "rgba(139, 92, 246, 0.6)",
    colorClass: "text-violet-500 dark:text-violet-400",
    empty: "No draft acceptance data yet",
  },
} satisfies Record<ActivityMetric, Record<string, string>>;

export interface ActivityChartPoint {
  model: string;
  x: number;
  y: number;
  tooltipLines: string[];
}

const number = new Intl.NumberFormat();
const decimal = new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });

function validPrompt(point: { model: string; input_tokens: number }): boolean {
  return point != null && typeof point.model === "string" &&
    Number.isSafeInteger(point.input_tokens) && point.input_tokens > 0;
}

export function preparePromptChartPoints(points: readonly PromptPoint[] | null | undefined): ActivityChartPoint[] {
  return preparePromptPoints(points).map((point) => ({
    model: point.model, x: point.x, y: point.y, tooltipLines: promptTooltipLines(point),
  }));
}

export function prepareGenerationPoints(points: readonly GenerationPoint[] | null | undefined): ActivityChartPoint[] {
  if (!Array.isArray(points)) return [];
  return points.flatMap((point) => {
    if (!validPrompt(point) || !Number.isFinite(point.tokens_per_second) || point.tokens_per_second <= 0) return [];
    return [{
      model: point.model, x: point.input_tokens, y: point.tokens_per_second,
      tooltipLines: [
        `Prompt: ${number.format(point.input_tokens)} tokens`,
        `Generation: ${decimal.format(point.tokens_per_second)} t/s`,
      ],
    }];
  });
}

export function prepareDraftPoints(points: readonly DraftPoint[] | null | undefined): ActivityChartPoint[] {
  if (!Array.isArray(points)) return [];
  return points.flatMap((point) => {
    if (!validPrompt(point) || !Number.isSafeInteger(point.draft_tokens) || point.draft_tokens <= 0 ||
      !Number.isSafeInteger(point.draft_acc_tokens) || point.draft_acc_tokens < 0) return [];
    const acceptance = 100 * point.draft_acc_tokens / point.draft_tokens;
    return [{
      model: point.model, x: point.input_tokens, y: acceptance,
      tooltipLines: [
        `Prompt: ${number.format(point.input_tokens)} tokens`,
        `Drafted: ${number.format(point.draft_tokens)} tokens`,
        `Accepted: ${number.format(point.draft_acc_tokens)} tokens`,
        `Acceptance: ${decimal.format(acceptance)}%`,
      ],
    }];
  });
}
