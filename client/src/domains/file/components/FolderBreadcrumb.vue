<script setup lang="ts">
import { computed } from "vue";
import { folderRoute, joinDir } from "../../../pkg/utils/folder_route.ts";
import type { MenuItem } from "primevue/menuitem";
import type { PathEntry } from "../models.ts";

const props = defineProps<{ path: PathEntry[] }>();

// The last item is the open folder, so it shows as text instead of a link.
const home = computed<MenuItem>(() => ({
    label: "Home",
    icon: "pi pi-home",
    route: folderRoute(""),
    current: props.path.length === 0,
}));

const items = computed<MenuItem[]>(() => {
    let dir = "";
    return props.path.map((entry, i) => {
        dir = joinDir(dir, entry.name);
        return { label: entry.name, route: folderRoute(dir), current: i === props.path.length - 1 };
    });
});
</script>

<template>
    <Breadcrumb :home="home" :model="items" class="min-w-0 bg-transparent p-0">
        <template #item="{ item }">
            <span
                v-if="item.current"
                aria-current="page"
                class="flex max-w-60 items-center gap-1.5 text-xs font-semibold text-fg"
                :title="String(item.label)"
            >
                <i v-if="item.icon" :class="item.icon" />
                <span class="truncate">{{ item.label }}</span>
            </span>
            <router-link
                v-else
                :to="item.route"
                class="flex max-w-40 items-center gap-1.5 rounded-md text-xs text-fg-muted no-underline hover:text-fg"
                :title="String(item.label)"
            >
                <i v-if="item.icon" :class="item.icon" />
                <span class="truncate">{{ item.label }}</span>
            </router-link>
        </template>
    </Breadcrumb>
</template>
