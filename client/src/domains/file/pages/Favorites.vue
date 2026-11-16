<script setup lang="ts">
import FavoritesPaginated from "../components/FavoritesPaginated.vue";
</script>

<template>
    <div class="flex flex-col gap-8">
        <div class="flex flex-col gap-1">
            <h1 class="text-xl font-semibold tracking-tight text-fg sm:text-2xl">Favorites</h1>
            <p class="text-sm text-fg-muted">Your favorite files and folders, from all folders.</p>
        </div>

        <FavoritesPaginated />
    </div>
</template>
