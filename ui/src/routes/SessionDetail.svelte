<script lang="ts">
  import { untrack } from "svelte";
  import { params } from "svelte-spa-router";
  import type { ActivityLogEntry, ActivityStatsData } from "../lib/types";
  import {
    activityRevision,
    getActivity,
    getActivityStats,
    inflightRequestEntries,
    uiConfig,
  } from "../stores/api";
  import { connectionState } from "../stores/theme";
  import { persistentStore } from "../stores/persistent";
  import {
    emptyActivityFilters,
    normalizeActivityFilters,
    type ActivityFilters,
  } from "../lib/activityFilters";
  import { sessionID } from "../lib/inflight";
  import ActivityStats from "../components/ActivityStats.svelte";
  import ActivityTable from "../components/ActivityTable.svelte";
  import * as Card from "$lib/components/ui/card/index.js";

  let sessionId = $derived($params?.id ?? "");

  const storedPageSize = persistentStore<number>("session-detail-activity-page-size", 25);
  const storedFilters = persistentStore<ActivityFilters>(
    "session-detail-activity-filters",
    emptyActivityFilters()
  );

  let rows = $state<ActivityLogEntry[]>([]);
  let stats = $state<ActivityStatsData | null>(null);
  let page = $state(1);
  let limit = $state($storedPageSize);
  let sort = $state("id");
  let order = $state<"asc" | "desc">("desc");
  let total = $state(0);
  let totalPages = $state(0);
  // svelte-ignore state_referenced_locally
  let filters = $state<ActivityFilters>(normalizeActivityFilters($storedFilters));
  let requestID = 0;
  let statsRequestID = 0;
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;
  let statsRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  let lastRefresh = 0;
  let lastStatsRefresh = 0;

  let sessionInflight = $derived(
    sessionId
      ? $inflightRequestEntries.filter(
          (request) => sessionID(request.req_headers, $uiConfig.activity.session_id) === sessionId
        )
      : []
  );

  async function refreshActivity() {
    if (refreshTimer !== null) {
      clearTimeout(refreshTimer);
      refreshTimer = null;
    }
    lastRefresh = Date.now();
    const id = ++requestID;
    const requestedSessionID = sessionId;
    if (!requestedSessionID) {
      rows = [];
      total = 0;
      totalPages = 0;
      return;
    }
    try {
      const activity = await getActivity({
        sessionID: requestedSessionID,
        page,
        limit,
        sort,
        order,
        filters,
      });
      if (id !== requestID || requestedSessionID !== sessionId) return;
      rows = activity.data;
      total = activity.total;
      totalPages = activity.total_pages;
    } catch (error) {
      if (id === requestID && requestedSessionID === sessionId) {
        console.error("Failed to refresh session activity:", error);
      }
    }
  }

  async function refreshStats() {
    if (statsRefreshTimer !== null) {
      clearTimeout(statsRefreshTimer);
      statsRefreshTimer = null;
    }
    lastStatsRefresh = Date.now();
    const id = ++statsRequestID;
    const requestedSessionID = sessionId;
    if (!requestedSessionID) {
      stats = null;
      return;
    }
    try {
      const activityStats = await getActivityStats(undefined, requestedSessionID);
      if (id !== statsRequestID || requestedSessionID !== sessionId) return;
      stats = activityStats;
    } catch (error) {
      if (id === statsRequestID && requestedSessionID === sessionId) {
        console.error("Failed to refresh session activity stats:", error);
      }
    }
  }

  function setPage(nextPage: number) {
    page = nextPage;
  }

  function setPageSize(nextLimit: number) {
    limit = nextLimit;
    page = 1;
    storedPageSize.set(nextLimit);
  }

  function setSort(nextSort: string, nextOrder: "asc" | "desc") {
    sort = nextSort;
    order = nextOrder;
    page = 1;
  }

  function setFilters(nextFilters: ActivityFilters) {
    filters = nextFilters;
    page = 1;
    storedFilters.set(nextFilters);
  }

  function scheduleRefresh() {
    if (refreshTimer !== null) return;
    const wait = Math.max(0, 1000 - (Date.now() - lastRefresh));
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      refreshActivity();
    }, wait);
  }

  function scheduleStatsRefresh() {
    if (statsRefreshTimer !== null) return;
    const wait = Math.max(0, 1000 - (Date.now() - lastStatsRefresh));
    statsRefreshTimer = setTimeout(() => {
      statsRefreshTimer = null;
      refreshStats();
    }, wait);
  }

  // A different route key starts at its first page and clears stale session data.
  $effect(() => {
    sessionId;
    untrack(() => {
      page = 1;
      rows = [];
      stats = null;
      total = 0;
      totalPages = 0;
      requestID++;
      statsRequestID++;
      if (refreshTimer !== null) clearTimeout(refreshTimer);
      if (statsRefreshTimer !== null) clearTimeout(statsRefreshTimer);
      refreshTimer = null;
      statsRefreshTimer = null;
    });
  });

  $effect(() => {
    if ($connectionState !== "connected") return;
    sessionId;
    page;
    limit;
    sort;
    order;
    filters;
    untrack(refreshActivity);
  });

  $effect(() => {
    if ($connectionState !== "connected") return;
    sessionId;
    untrack(refreshStats);
  });

  let seenRevision = $activityRevision;
  $effect(() => {
    if ($connectionState !== "connected") return;
    const revision = $activityRevision;
    untrack(() => {
      if (revision === seenRevision) return;
      seenRevision = revision;
      scheduleStatsRefresh();
      if (page === 1) scheduleRefresh();
    });
  });

  $effect(() => {
    return () => {
      if (refreshTimer !== null) clearTimeout(refreshTimer);
      if (statsRefreshTimer !== null) clearTimeout(statsRefreshTimer);
      requestID++;
      statsRequestID++;
    };
  });
</script>

<div class="flex h-full flex-col gap-3 overflow-y-auto p-2">
  <Card.Root class="shrink-0 p-4">
    <Card.Title class="text-lg">Session ID</Card.Title>
    <p class="mt-2 break-all font-mono text-sm">{sessionId || "-"}</p>
  </Card.Root>

  <ActivityStats {stats} />

  <ActivityTable
    metrics={rows}
    inflightRequests={sessionInflight}
    storagePrefix="session-detail-activity"
    showModelColumn={true}
    showPagination={true}
    {page}
    {limit}
    {total}
    totalPages={totalPages}
    onPageChange={setPage}
    onPageSizeChange={setPageSize}
    {sort}
    {order}
    onSortChange={setSort}
    {filters}
    onFiltersChange={setFilters}
    cardClass="min-h-[30rem] overflow-auto"
    emptyMessage="No activity for this session"
  />
</div>
