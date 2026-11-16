<script setup lang="ts">
import { computed } from "vue";
import { useRoute } from "vue-router";

interface SettingsLink {
    name: string;
    label: string;
    description: string;
    icon: string;
}

const SETTINGS_LINKS: SettingsLink[] = [
    {
        name: "settings.files",
        label: "Files",
        description: "How your file lists sort and show items. Each change saves at once.",
        icon: "pi pi-folder",
    },
];

const route = useRoute();

const activeLink = computed<SettingsLink | undefined>(() => SETTINGS_LINKS.find((link) => link.name === route.name));
</script>

<template>
    <div class="flex flex-col gap-8">
        <div class="flex flex-col gap-1">
            <span class="text-xs font-semibold tracking-wide text-fg-muted uppercase">Settings</span>
            <h1 class="text-xl font-semibold tracking-tight text-fg sm:text-2xl">{{ activeLink?.label }}</h1>
            <p class="text-sm text-fg-muted">{{ activeLink?.description }}</p>
        </div>

        <div class="grid gap-5 lg:grid-cols-[200px_minmax(0,1fr)] lg:gap-8">
            <nav
                class="flex gap-0.5 overflow-x-auto p-px pb-1 lg:sticky lg:top-24 lg:flex-col lg:self-start lg:overflow-visible lg:p-0"
                aria-label="Settings"
            >
                <router-link
                    v-for="link in SETTINGS_LINKS"
                    :key="link.name"
                    :to="{ name: link.name }"
                    class="group flex h-9 shrink-0 items-center gap-3 rounded-lg px-3 text-sm font-medium text-fg-muted no-underline transition-colors hover:bg-inset hover:text-fg aria-[current=page]:bg-panel aria-[current=page]:text-fg aria-[current=page]:shadow-sm aria-[current=page]:ring-1 aria-[current=page]:ring-edge"
                >
                    <i :class="link.icon" class="text-sm group-aria-[current=page]:text-accent" />
                    <span>{{ link.label }}</span>
                </router-link>
            </nav>

            <section class="min-w-0">
                <router-view />
            </section>
        </div>
    </div>
</template>
