<script setup lang="ts">
import { reactive, ref } from "vue";
import { useSettingsStore } from "../../../../pkg/stores/settings_store.ts";
import { useToastStore } from "../../../../pkg/stores/toast_store.ts";
import type { Settings } from "../../models.ts";

interface SettingRow {
    key: keyof Settings;
    label: string;
    description: string;
    icon: string;
}

interface SettingGroup {
    title: string;
    rows: SettingRow[];
}

const SETTING_GROUPS: SettingGroup[] = [
    {
        title: "Sort",
        rows: [
            {
                key: "sort_favorites_first",
                label: "Sort favorites first",
                description: "Favorites come before the other items in Files.",
                icon: "pi pi-star",
            },
            {
                key: "sort_folders_first",
                label: "Sort folders before files",
                description: "Folders come before files in Files, Favorites and Trash.",
                icon: "pi pi-folder",
            },
        ],
    },
    {
        title: "Show",
        rows: [
            {
                key: "show_hidden",
                label: "Show hidden files",
                description: "Files and Favorites open with Show hidden on.",
                icon: "pi pi-eye-slash",
            },
            {
                key: "show_extensions",
                label: "Show file extensions",
                description: "When off, report.pdf shows as report.",
                icon: "pi pi-file",
            },
        ],
    },
];

const settingsStore = useSettingsStore();
const toastStore = useToastStore();

// The switches show a copy of the settings. A switch keeps its own state, so after each save the copy takes the
// stored settings again: a failed save turns the switch back.
const form = reactive<Settings>({ ...settingsStore.settings });

// The switches wait while a save runs, so two saves never cross.
const saving = ref<boolean>(false);

async function save(): Promise<void> {
    saving.value = true;
    try {
        await settingsStore.save({ ...form });
    } catch (error) {
        toastStore.errorResponseToast(error);
    } finally {
        Object.assign(form, settingsStore.settings);
        saving.value = false;
    }
}
</script>

<template>
    <div class="mx-auto flex max-w-2xl flex-col gap-6">
        <section v-for="group in SETTING_GROUPS" :key="group.title" class="flex flex-col gap-2">
            <h2 class="px-1 text-xs font-semibold tracking-wide text-fg-muted uppercase">{{ group.title }}</h2>
            <div class="divide-y divide-edge overflow-hidden rounded-2xl border border-edge bg-panel">
                <label
                    v-for="row in group.rows"
                    :key="row.key"
                    :for="row.key"
                    class="flex cursor-pointer items-center gap-4 px-4 py-3.5 sm:px-5"
                >
                    <span class="flex size-9 shrink-0 items-center justify-center rounded-xl bg-inset text-fg-muted">
                        <i :class="row.icon" class="text-sm" />
                    </span>
                    <span class="flex min-w-0 flex-1 flex-col gap-0.5">
                        <span class="text-sm font-medium text-fg">{{ row.label }}</span>
                        <span class="text-xs text-fg-muted">{{ row.description }}</span>
                    </span>
                    <ToggleSwitch
                        v-model="form[row.key]"
                        :input-id="row.key"
                        :disabled="saving"
                        class="shrink-0"
                        @update:model-value="save"
                    />
                </label>
            </div>
        </section>
    </div>
</template>
