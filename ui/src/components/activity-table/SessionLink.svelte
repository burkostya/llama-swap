<script lang="ts">
  import { link } from "svelte-spa-router";
  import MiddleEllipsis from "./MiddleEllipsis.svelte";
  import { sessionPath } from "../../lib/inflight";

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();
  let href = $derived(sessionPath(sessionId));
</script>

{#if href}
  <a href={href} use:link class="text-primary hover:underline" title={sessionId}>
    <MiddleEllipsis value={sessionId} tailLength={8} className="max-w-[14rem] font-mono text-xs" />
  </a>
{:else}
  <span class="text-muted-foreground">-</span>
{/if}
