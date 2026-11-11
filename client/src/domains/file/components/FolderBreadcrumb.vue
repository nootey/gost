<script setup lang="ts">
import { computed } from "vue";
import type { MenuItem } from "primevue/menuitem";
import type { PathEntry } from "../models.ts";

const props = defineProps<{ path: PathEntry[] }>();

// The last item is the open folder, so it shows as text instead of a link.
const home = computed<MenuItem>(() => ({
    label: "Files",
    route: { name: "dashboard" },
    current: props.path.length === 0,
}));

const items = computed<MenuItem[]>(() =>
    props.path.map((entry, i) => ({
        label: entry.name,
        route: { name: "folder", params: { id: entry.id } },
        current: i === props.path.length - 1,
    })),
);
</script>

<template>
    <Breadcrumb :home="home" :model="items" class="min-w-0 bg-transparent p-0">
        <template #item="{ item }">
            <span
                v-if="item.current"
                aria-current="page"
                class="block max-w-60 truncate text-xl font-semibold tracking-tight text-fg sm:text-2xl"
                :title="String(item.label)"
            >
                {{ item.label }}
            </span>
            <router-link
                v-else
                :to="item.route"
                class="block max-w-40 truncate rounded-md text-xl tracking-tight text-fg-muted no-underline hover:text-fg sm:text-2xl"
                :title="String(item.label)"
            >
                {{ item.label }}
            </router-link>
        </template>
    </Breadcrumb>
</template>
