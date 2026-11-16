<script setup lang="ts">
import ActivityPaginated from "../components/ActivityPaginated.vue";
</script>

<template>
    <div class="flex flex-col gap-8">
        <div class="flex flex-col gap-1">
            <h1 class="text-xl font-semibold tracking-tight text-fg sm:text-2xl">Activity</h1>
            <p class="text-sm text-fg-muted">What happened to your files and folders, newest first.</p>
        </div>

        <ActivityPaginated />
    </div>
</template>
