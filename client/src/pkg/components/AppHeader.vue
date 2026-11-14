<script setup lang="ts">
import { computed, ref } from "vue";
import { storeToRefs } from "pinia";
import { useRoute } from "vue-router";
import { useConfirm } from "primevue/useconfirm";
import { useAuthStore } from "../stores/auth_store.ts";
import { useThemeStore } from "../stores/theme_store.ts";
import BrandMark from "./BrandMark.vue";
import type { MenuItem } from "primevue/menuitem";

interface NavLink {
    name: string;
    label: string;
    icon: string;
}

const NAV_LINKS: NavLink[] = [
    { name: "dashboard", label: "Files", icon: "pi pi-folder" },
    { name: "trash", label: "Trash", icon: "pi pi-trash" },
];

const authStore = useAuthStore();
const themeStore = useThemeStore();
const { user } = storeToRefs(authStore);
const confirm = useConfirm();
const route = useRoute();

const darkMode = computed<boolean>({
    get: () => themeStore.isDark,
    set: (value) => themeStore.setTheme(value ? "dark" : "light"),
});

const menuRef = ref<{ toggle: (event: Event) => void } | null>(null);

const menuItems: MenuItem[] = [{ label: "Sign out", icon: "pi pi-sign-out", command: signOut }];

const initials = computed<string>(() =>
    (user.value?.display_name ?? "")
        .split(" ")
        .filter(Boolean)
        .map((part) => part[0])
        .slice(0, 2)
        .join("")
        .toUpperCase(),
);

function toggleMenu(event: Event): void {
    menuRef.value?.toggle(event);
}

function signOut(): void {
    confirm.require({
        header: "Sign out",
        message: "Do you want to sign out?",
        rejectProps: { label: "Cancel" },
        acceptProps: { label: "Sign out", severity: "danger" },
        accept: () => authStore.logoutUser(),
    });
}
</script>

<template>
    <div class="flex h-16 items-center justify-between gap-4">
        <div class="flex min-w-0 items-center gap-3 sm:gap-6">
            <router-link to="/" class="no-underline" aria-label="Gost home">
                <BrandMark />
            </router-link>

            <nav class="flex items-center gap-1" aria-label="Main">
                <Button
                    v-for="link in NAV_LINKS"
                    :key="link.name"
                    as="router-link"
                    :to="{ name: link.name }"
                    text
                    size="small"
                    severity="secondary"
                    class="gap-2 no-underline"
                    :class="{ 'bg-inset text-fg': route.name === link.name }"
                    :aria-label="link.label"
                >
                    <i :class="link.icon" />
                    <span class="hidden sm:inline">{{ link.label }}</span>
                </Button>
            </nav>
        </div>

        <Button
            text
            rounded
            severity="secondary"
            class="gap-2 p-1 sm:pr-3"
            aria-haspopup="true"
            aria-controls="account-menu"
            aria-label="Account menu"
            @click="toggleMenu"
        >
            <Avatar :label="initials" shape="circle" class="bg-accent-soft text-xs font-semibold text-accent" />
            <span class="hidden max-w-40 truncate text-sm font-medium text-fg sm:block">
                {{ user?.display_name }}
            </span>
            <i class="pi pi-chevron-down hidden text-xs text-fg-muted sm:block" />
        </Button>

        <Menu id="account-menu" ref="menuRef" :model="menuItems" popup class="w-64">
            <template #start>
                <div class="flex flex-col gap-0.5 border-b border-edge px-3 py-3">
                    <span class="truncate font-medium text-fg">{{ user?.display_name }}</span>
                    <span class="truncate text-xs text-fg-muted">{{ user?.email }}</span>
                </div>
                <label
                    for="dark-mode-switch"
                    class="flex h-11 cursor-pointer items-center justify-between gap-3 border-b border-edge px-3 text-sm text-fg"
                >
                    <span class="flex items-center gap-3">
                        <i class="pi pi-moon text-sm text-fg-muted" />
                        Dark mode
                    </span>
                    <ToggleSwitch v-model="darkMode" input-id="dark-mode-switch" />
                </label>
            </template>
        </Menu>
    </div>
</template>
