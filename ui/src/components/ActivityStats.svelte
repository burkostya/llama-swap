<script lang="ts">
  import type { ActivityStatsData } from "../lib/types";
  import { persistentStore } from "../stores/persistent";
  import { preparePromptPoints } from "../lib/promptScatter";
  import HeaderLabel from "./activity-table/HeaderLabel.svelte";
  import PromptScatter from "./PromptScatter.svelte";
  import TokenHistogram from "./TokenHistogram.svelte";
  import { ChevronDown, X } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Button } from "$lib/components/ui/button/index.js";

  interface Props {
    stats: ActivityStatsData | null;
  }

  let { stats }: Props = $props();

  const nf = new Intl.NumberFormat();
  const histogramCollapsed = persistentStore<boolean>("activity-histogram-collapsed", false);
  let promptPoints = $derived(preparePromptPoints(stats?.prompt_points));
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
    <div class="mb-3 grid grid-cols-1 gap-6 lg:grid-cols-3">
      <div class="w-full min-w-0">
        <div class="text-muted-foreground mb-1 text-sm font-medium">
          <HeaderLabel
            label="Prompt Processing"
            tooltip="Prefill speed by prompt size. Token counts match the Prompt column; cache accounting depends on the backend."
          />
        </div>
        {#if promptPoints.length > 0}
          <PromptScatter points={promptPoints} />
        {:else}
          <div class="text-muted-foreground py-6 text-center text-sm">No prompt speed data yet</div>
        {/if}
      </div>
      <div class="w-full min-w-0">
        <div class="text-muted-foreground mb-1 text-sm font-medium">Token Generation</div>
        {#if stats?.gen_histogram}
          <TokenHistogram data={stats.gen_histogram} unit="tokens/sec" />
        {:else}
          <div class="text-muted-foreground py-6 text-center text-sm">No generation speed data yet</div>
        {/if}
      </div>
      <div class="w-full min-w-0">
        <div class="text-muted-foreground mb-1 text-sm font-medium">Draft Acceptance</div>
        {#if stats?.draft_histogram}
          <TokenHistogram
            data={stats.draft_histogram}
            unit="acceptance rate (%)"
            colorClass="text-violet-500 dark:text-violet-400"
          />
        {:else}
          <div class="text-muted-foreground py-6 text-center text-sm">No draft acceptance data yet</div>
        {/if}
      </div>
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
