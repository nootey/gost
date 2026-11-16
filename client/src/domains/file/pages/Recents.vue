<script setup lang="ts">
import RecentFiles from "../components/RecentFiles.vue";
</script>

<template>
    <div class="flex flex-col gap-8">
        <div class="flex flex-col gap-1">
            <h1 class="text-xl font-semibold tracking-tight text-fg sm:text-2xl">Recent</h1>
            <p class="text-sm text-fg-muted">The 10 files you added or changed last, from all folders.</p>
        </div>

        <RecentFiles />
    </div>
</template>
