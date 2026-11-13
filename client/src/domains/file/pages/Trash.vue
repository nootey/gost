<script setup lang="ts">
import TrashPaginated from "../components/TrashPaginated.vue";
</script>

<template>
    <div class="mx-auto flex w-full max-w-4xl flex-col gap-8 px-4 py-10">
        <div class="flex flex-col gap-1">
            <h1 class="text-xl font-semibold tracking-tight text-fg sm:text-2xl">Trash</h1>
            <p class="text-sm text-fg-muted">Items in the trash are deleted for good after 30 days.</p>
        </div>

        <TrashPaginated />
    </div>
</template>
