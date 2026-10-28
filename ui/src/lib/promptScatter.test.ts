import { describe, expect, it } from "vitest";
import type { PromptPoint } from "./types";
import { formatPromptTick, preparePromptPoints, promptLogBounds, promptTooltipLines } from "./promptScatter";

const short: PromptPoint = { model: "short", input_tokens: 20, cache_tokens: 35814, prompt_per_second: 30.2 };
const medium: PromptPoint = { model: "medium", input_tokens: 1000, cache_tokens: 0, prompt_per_second: 300 };
const long: PromptPoint = { model: "long", input_tokens: 10000, cache_tokens: -1, prompt_per_second: 1000 };

describe("prompt scatter", () => {
  it("preserves every observation and its original coordinates", () => {
    const points = preparePromptPoints([
      ...Array(80).fill(short),
      ...Array(15).fill(medium),
      ...Array(5).fill(long),
    ]);
    expect(points).toHaveLength(100);
    expect(points[0]).toEqual({ ...short, x: 20, y: 30.2 });
    expect(points[80]).toEqual({ ...medium, x: 1000, y: 300 });
    expect(points[95]).toEqual({ ...long, x: 10000, y: 1000 });
    expect(promptLogBounds(points)).toEqual({ min: 10, max: 10000 });
    // Equal multiples have equal distance on a logarithmic axis.
    expect(Math.log10(points[95].x) - Math.log10(points[80].x)).toBe(1);
  });

  it("handles empty, single-point and identical data", () => {
    expect(preparePromptPoints(undefined)).toEqual([]);
    expect(promptLogBounds([])).toEqual({ min: 1, max: 10 });
    expect(promptLogBounds(preparePromptPoints([medium]))).toEqual({ min: 1000, max: 10000 });
    expect(promptLogBounds(preparePromptPoints([short, short]))).toEqual({ min: 10, max: 100 });
    expect(promptLogBounds(preparePromptPoints([{ ...short, input_tokens: 1 }]))).toEqual({ min: 1, max: 10 });
  });

  it("ignores malformed points and nonpositive or nonfinite coordinates", () => {
    const points = [
      null,
      { ...short, input_tokens: 0 },
      { ...short, input_tokens: -1 },
      { ...short, input_tokens: 1.5 },
      { ...short, input_tokens: Infinity },
      { ...short, prompt_per_second: 0 },
      { ...short, prompt_per_second: -1 },
      { ...short, prompt_per_second: NaN },
      { ...short, prompt_per_second: Infinity },
      { ...short, model: null },
      short,
    ] as unknown as PromptPoint[];
    expect(preparePromptPoints(points)).toEqual([{ ...short, x: 20, y: 30.2 }]);
    expect(preparePromptPoints({} as PromptPoint[])).toEqual([]);
  });

  it("formats compact ticks but uses exact values in tooltips", () => {
    expect(formatPromptTick(1000)).toBe(new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(1000));
    expect(formatPromptTick(2000)).toBe("");
    expect(formatPromptTick(0)).toBe("");
    expect(promptTooltipLines({ ...short, input_tokens: 1567, prompt_per_second: 313.456 })).toEqual([
      `Prompt: ${new Intl.NumberFormat().format(1567)} tokens`,
      `Cached: ${new Intl.NumberFormat().format(35814)} tokens`,
      `Prefill: ${new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(313.456)} t/s`,
    ]);
    expect(promptTooltipLines(long)[1]).toBe("Cached: Unknown");
    expect(promptTooltipLines(medium)[1]).toBe("Cached: 0 tokens");
  });
});
