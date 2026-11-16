<script setup lang="ts">
import { computed, ref } from "vue";
import { useRegle } from "@regle/core";
import { required } from "@regle/rules";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import ValidationError from "../../../pkg/components/ValidationError.vue";
import type { NewItemForm, NewItemKind, StoredFile } from "../models.ts";

interface NewItemView {
    header: string;
    placeholder: string;
    hint: string;
}

interface CreatedItem {
    title: string;
    message: string;
    item: StoredFile;
}

const VIEWS: Record<NewItemKind, NewItemView> = {
    folder: { header: "New folder", placeholder: "Folder name", hint: "" },
    file: { header: "New file", placeholder: "File name, such as notes.txt", hint: "The file starts empty." },
};

const props = defineProps<{ folderId: number | null; kind: NewItemKind }>();
const visible = defineModel<boolean>("visible", { required: true });
const emit = defineEmits<{ created: [item: StoredFile] }>();

const fileStore = useFileStore();
const toastStore = useToastStore();

const CREATE_ACTIONS: Record<NewItemKind, (form: NewItemForm) => Promise<CreatedItem>> = {
    folder: async (form) => {
        const response = await fileStore.createFolder(form);
        return { ...response, item: response.folder };
    },
    file: async (form) => {
        const response = await fileStore.createFile(form);
        return { ...response, item: response.file };
    },
};

const loading = ref<boolean>(false);
const view = computed<NewItemView>(() => VIEWS[props.kind]);

const form = ref<NewItemForm>({
    name: "",
    parent_id: props.folderId,
});

const { r$ } = useRegle(form, {
    name: {
        required,
    },
});

// Start empty each time, inside whatever folder is open now.
function reset(): void {
    r$.$reset({ toState: { name: "", parent_id: props.folderId } });
}

// Enter can fire again while a create is running; a second call would make "Name (1)".
async function create(): Promise<void> {
    if (loading.value) return;
    await r$.$validate();
    if (r$.$invalid) return;

    loading.value = true;
    try {
        const response = await CREATE_ACTIONS[props.kind](form.value);
        toastStore.successResponseToast(response);
        emit("created", response.item);
        visible.value = false;
    } catch (error) {
        toastStore.errorResponseToast(error);
    } finally {
        loading.value = false;
    }
}
</script>

<template>
    <Dialog
        v-model:visible="visible"
        modal
        :header="view.header"
        :draggable="false"
        class="w-[min(420px,calc(100vw-2rem))] rounded-2xl border-edge bg-panel"
        @show="reset"
    >
        <div class="flex flex-col gap-1">
            <ValidationError :is-required="true" :message="r$.name.$errors[0]">
                <label for="new-item-name">Name</label>
            </ValidationError>
            <InputText
                id="new-item-name"
                v-model="form.name"
                :placeholder="view.placeholder"
                class="w-full rounded-xl"
                autofocus
                @keydown.enter="create"
            />
            <span v-if="view.hint" class="text-xs text-fg-muted">{{ view.hint }}</span>
        </div>

        <template #footer>
            <Button label="Cancel" severity="secondary" variant="outlined" @click="visible = false" />
            <Button
                :label="loading ? 'Creating...' : 'Create'"
                :icon="loading ? 'pi pi-spin pi-spinner mr-2' : ''"
                :disabled="loading"
                @click="create"
            />
        </template>
    </Dialog>
</template>
