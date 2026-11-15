<script setup lang="ts">
import { computed, onMounted, watch } from "vue";
import { storeToRefs } from "pinia";
import { useRoute } from "vue-router";
import { useAuthStore } from "./pkg/stores/auth_store.ts";
import { useDownloadStore } from "./pkg/stores/download_store.ts";
import { useThemeStore } from "./pkg/stores/theme_store.ts";
import AppHeader from "./pkg/components/AppHeader.vue";

const authStore = useAuthStore();
const themeStore = useThemeStore();
const downloadStore = useDownloadStore();
const route = useRoute();

themeStore.initializeTheme();

const { isAuthenticated, isInitialized, user } = storeToRefs(authStore);

const requiresAuthView = computed<boolean>(() => route.matched.some((r) => r.meta.requiresAuth));
const hideNavigation = computed<boolean>(() => route.matched.some((r) => r.meta.hideNavigation));
const showShell = computed<boolean>(() => isAuthenticated.value && isInitialized.value && !hideNavigation.value);

const SHELL_WIDTH = "mx-auto w-full px-4 lg:w-3/4";

// The downloads list belongs to the signed-in user. It loads when a user signs in or the page loads, and it clears on
// logout.
watch(
    () => user.value?.id,
    (id) => {
        if (id === undefined) downloadStore.clear();
        else void downloadStore.load();
    },
);

onMounted(async () => {
    if (isAuthenticated.value) await authStore.init();
});
</script>

<template>
    <Toast position="bottom-center" group="bc" />
    <Toast position="bottom-right" group="br" />
    <ConfirmDialog unstyled>
        <template #container="{ message, acceptCallback, rejectCallback }">
            <div class="p-overlay-mask p-overlay-mask-enter flex items-center justify-center">
                <div
                    class="flex w-[min(420px,calc(100vw-2rem))] flex-col gap-4 rounded-2xl border border-edge bg-panel p-6 shadow-2xl"
                >
                    <div class="text-lg font-semibold tracking-tight text-fg">
                        {{ message.header }}
                    </div>
                    <div class="whitespace-pre-line text-sm leading-relaxed text-fg-muted">
                        {{ message.message }}
                    </div>
                    <div class="flex justify-end gap-2 pt-2">
                        <Button
                            :label="message.rejectProps?.label || 'Cancel'"
                            severity="secondary"
                            variant="outlined"
                            @click="rejectCallback"
                        />
                        <Button
                            :label="message.acceptProps?.label || 'Confirm'"
                            :severity="message.acceptProps?.severity"
                            @click="acceptCallback"
                        />
                    </div>
                </div>
            </div>
        </template>
    </ConfirmDialog>

    <div class="flex min-h-screen flex-col">
        <header v-if="showShell" class="sticky top-0 z-30 border-b border-edge bg-page/80 backdrop-blur">
            <AppHeader :class="SHELL_WIDTH" />
        </header>
        <main class="flex flex-1 flex-col">
            <div v-if="requiresAuthView && !isInitialized" class="flex flex-1 items-center justify-center">
                <i class="pi pi-spin pi-spinner text-2xl text-fg-muted" />
            </div>
            <div v-else-if="showShell" :class="[SHELL_WIDTH, 'py-10']">
                <router-view />
            </div>
            <router-view v-else />
        </main>
    </div>
</template>
