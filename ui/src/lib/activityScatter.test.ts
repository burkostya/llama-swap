import { describe, expect, it } from "vitest";
import type { DraftPoint, GenerationPoint } from "./types";
import { normalizeChartType, prepareDraftPoints, prepareGenerationPoints, preparePromptChartPoints } from "./activityScatter";
import { promptLogBounds } from "./promptScatter";

const generation: GenerationPoint = { model: "m", input_tokens: 1000, tokens_per_second: 42.345 };
const draft: DraftPoint = { model: "m", input_tokens: 1000, draft_tokens: 20, draft_acc_tokens: 15 };
const nf = new Intl.NumberFormat();
const decimal = new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });

describe("activity scatter", () => {
  it("uses prompt size for all metrics and preserves coincident observations", () => {
    const points = prepareGenerationPoints([generation, generation, { ...generation, input_tokens: 10000 }]);
    expect(points).toHaveLength(3);
    expect(points[0]).toMatchObject({ model: "m", x: 1000, y: 42.345 });
    expect(points[1]).toEqual(points[0]);
    expect(promptLogBounds(points)).toEqual({ min: 1000, max: 10000 });
    expect(promptLogBounds(points.slice(0, 1))).toEqual({ min: 1000, max: 10000 });
    expect(promptLogBounds([])).toEqual({ min: 1, max: 10 });
    expect(prepareDraftPoints([draft])[0]).toMatchObject({ x: 1000, y: 75 });
    const prompt = preparePromptChartPoints([{ model: "m", input_tokens: 1000, cache_tokens: 5000, prompt_per_second: 100 }]);
    expect(prompt[0]).toMatchObject({ model: "m", x: 1000, y: 100 });
    expect(prompt[0].tooltipLines).toContain(`Cached: ${nf.format(5000)} tokens`);
  });

  it("keeps zero and above-100 draft acceptance and formats detailed tooltips", () => {
    const points = prepareDraftPoints([draft, { ...draft, draft_acc_tokens: 0 }, { ...draft, draft_acc_tokens: 25 }, draft]);
    expect(points.map((p) => p.y)).toEqual([75, 0, 125, 75]);
    expect(points[0].tooltipLines).toEqual([
      `Prompt: ${nf.format(1000)} tokens`, "Drafted: 20 tokens", "Accepted: 15 tokens", `Acceptance: ${decimal.format(75)}%`,
    ]);
    expect(prepareGenerationPoints([generation])[0].tooltipLines).toEqual([
      `Prompt: ${nf.format(1000)} tokens`, `Generation: ${decimal.format(42.345)} t/s`,
    ]);
  });

  it("handles absent arrays and rejects malformed or invalid observations", () => {
    expect(prepareGenerationPoints(undefined)).toEqual([]);
    expect(prepareDraftPoints(null)).toEqual([]);
    expect(preparePromptChartPoints(undefined)).toEqual([]);
    expect(prepareDraftPoints({} as DraftPoint[])).toEqual([]);
    expect(prepareGenerationPoints({} as GenerationPoint[])).toEqual([]);
    for (const patch of [
      null, { model: null }, { input_tokens: 0 }, { input_tokens: -1 }, { input_tokens: 1.5 },
      { input_tokens: Infinity }, { input_tokens: Number.MAX_SAFE_INTEGER + 1 },
    ]) {
      const g = patch === null ? null : { ...generation, ...patch };
      const d = patch === null ? null : { ...draft, ...patch };
      expect(prepareGenerationPoints([g] as GenerationPoint[])).toEqual([]);
      expect(prepareDraftPoints([d] as DraftPoint[])).toEqual([]);
    }
    for (const value of [0, -1, NaN, Infinity, -Infinity]) {
      expect(prepareGenerationPoints([{ ...generation, tokens_per_second: value }])).toEqual([]);
    }
    for (const value of [0, -1, 1.5, Infinity, NaN]) {
      expect(prepareDraftPoints([{ ...draft, draft_tokens: value }])).toEqual([]);
    }
    for (const value of [-1, 1.5, Infinity, NaN]) {
      expect(prepareDraftPoints([{ ...draft, draft_acc_tokens: value }])).toEqual([]);
    }
  });

  it("defaults invalid saved chart preferences to scatter", () => {
    expect(normalizeChartType("histogram")).toBe("histogram");
    for (const value of ["scatter", "invalid", "", undefined, null, {}, false]) {
      expect(normalizeChartType(value)).toBe("scatter");
    }
  });
});
