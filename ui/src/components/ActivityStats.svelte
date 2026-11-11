<script lang="ts">
  import type { ActivityStatsData } from "../lib/types";
  import { persistentStore } from "../stores/persistent";
  import { preparePromptChartPoints, prepareGenerationPoints, prepareDraftPoints } from "../lib/activityScatter";
  import ActivityMetricChart from "./ActivityMetricChart.svelte";
  import { ChevronDown, X } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Button } from "$lib/components/ui/button/index.js";

  interface Props {
    stats: ActivityStatsData | null;
  }

  let { stats }: Props = $props();

  const nf = new Intl.NumberFormat();
  const histogramCollapsed = persistentStore<boolean>("activity-histogram-collapsed", false);
  let promptPoints = $derived(preparePromptChartPoints(stats?.prompt_points));
  let genPoints = $derived(prepareGenerationPoints(stats?.gen_points));
  let draftPoints = $derived(prepareDraftPoints(stats?.draft_points));
</script>

<Card.Root class="relative p-3">
  <Button
    variant="ghost"
    size="icon-xs"
    class="text-muted-foreground absolute right-2 top-2 rounded-full"
    onclick={() => ($histogramCollapsed = !$histogramCollapsed)}
    title={$histogramCollapsed ? "Show charts" : "Hide charts"}
  >
    {#if $histogramCollapsed}
      <ChevronDown />
    {:else}
      <X />
    {/if}
  </Button>
  {#if !$histogramCollapsed}
    <div class="mb-3 grid grid-cols-1 gap-6 pt-5 lg:grid-cols-3">
      <ActivityMetricChart metric="prompt" points={promptPoints} histogram={stats?.prompt_histogram} />
      <ActivityMetricChart metric="generation" points={genPoints} histogram={stats?.gen_histogram} />
      <ActivityMetricChart metric="draft" points={draftPoints} histogram={stats?.draft_histogram} />
    </div>
  {/if}
  <div class="grid grid-cols-4 gap-x-6 gap-y-1 text-sm">
    <div class="text-muted-foreground text-xs uppercase tracking-wider">Requests</div>
    <div class="text-muted-foreground text-xs uppercase tracking-wider">Cached</div>
    <div class="text-muted-foreground text-xs uppercase tracking-wider">Processed</div>
    <div class="text-muted-foreground text-xs uppercase tracking-wider">Generated</div>
    <div class="text-sm">
      <span class="font-semibold">{nf.format(stats?.total_requests ?? 0)}</span> completed
    </div>
    <div class="text-sm">
      <span class="font-semibold">{nf.format(stats?.total_cache_tokens ?? 0)}</span> tokens
    </div>
    <div class="text-sm">
      <span class="font-semibold">{nf.format(stats?.total_input_tokens ?? 0)}</span> tokens
    </div>
    <div class="text-sm">
      <span class="font-semibold">{nf.format(stats?.total_output_tokens ?? 0)}</span> tokens
    </div>
  </div>
</Card.Root>
