<script lang="ts">
  import { untrack } from "svelte";
  import type { HistogramData } from "../lib/types";
  import { activityChartSettings, normalizeChartType, type ActivityChartPoint, type ActivityMetric } from "../lib/activityScatter";
  import { persistentStore } from "../stores/persistent";
  import HeaderLabel from "./activity-table/HeaderLabel.svelte";
  import ActivityScatter from "./ActivityScatter.svelte";
  import TokenHistogram from "./TokenHistogram.svelte";
  import { Button } from "$lib/components/ui/button/index.js";

  let { metric, points, histogram }: {
    metric: ActivityMetric;
    points: ActivityChartPoint[];
    histogram: HistogramData | null | undefined;
  } = $props();

  let settings = $derived(activityChartSettings[metric]);
  const storedType = untrack(() => persistentStore<unknown>(`activity-${metric}-chart-type`, "scatter"));
  let chartType = $derived(normalizeChartType($storedType));
  $effect(() => {
    if ($storedType !== chartType) $storedType = chartType;
  });
</script>

<div class="w-full min-w-0">
  <div class="mb-1 flex flex-wrap items-center justify-between gap-2">
    <div class="text-muted-foreground text-sm font-medium">
      <HeaderLabel label={settings.title} tooltip={settings.tooltip} />
    </div>
    <div class="flex" role="group" aria-label="{settings.title} chart type">
      {#each ["scatter", "histogram"] as type}
        <Button
          size="sm"
          variant={chartType === type ? "secondary" : "ghost"}
          class="h-7 px-2 text-xs"
          aria-pressed={chartType === type}
          onclick={() => ($storedType = type)}
        >{type === "scatter" ? "Scatter" : "Histogram"}</Button>
      {/each}
    </div>
  </div>
  {#if chartType === "scatter" && points.length > 0}
    <ActivityScatter {points} {metric} />
  {:else if chartType === "histogram" && histogram}
    <TokenHistogram data={histogram} unit={settings.unit} colorClass={settings.colorClass} />
  {:else}
    <div class="text-muted-foreground py-6 text-center text-sm">{settings.empty}</div>
  {/if}
</div>
