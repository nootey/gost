<script setup lang="ts">
import { computed, nextTick, ref } from "vue";
import { useRegle } from "@regle/core";
import { required } from "@regle/rules";
import { useFileStore } from "../../../pkg/stores/file_store.ts";
import { useToastStore } from "../../../pkg/stores/toast_store.ts";
import ValidationError from "../../../pkg/components/ValidationError.vue";
import vueHelper from "../../../pkg/utils/vue_helper.ts";
import type { RenameForm, StoredFile } from "../models.ts";

const props = defineProps<{ item: StoredFile | null }>();
const visible = defineModel<boolean>("visible", { required: true });
const emit = defineEmits<{ renamed: [item: StoredFile] }>();

const fileStore = useFileStore();
const toastStore = useToastStore();

const loading = ref<boolean>(false);
const inputRef = ref<{ $el: HTMLInputElement } | null>(null);

const form = ref<RenameForm>({
    name: "",
});

const { r$ } = useRegle(form, {
    name: {
        required,
    },
});

const HEADERS: Record<`${boolean}`, string> = {
    true: "Rename folder",
    false: "Rename file",
};

const header = computed<string>(() => HEADERS[`${props.item?.is_folder ?? false}`]);

async function reset(): Promise<void> {
    const name = props.item?.name ?? "";
    r$.$reset({ toState: { name } });
    await nextTick();
    inputRef.value?.$el.setSelectionRange(0, vueHelper.baseNameLength(name, props.item?.is_folder ?? false));
}

async function rename(): Promise<void> {
    if (loading.value || props.item === null) return;
    await r$.$validate();
    if (r$.$invalid) return;

    loading.value = true;
    try {
        const response = await fileStore.rename(props.item.id, form.value);
        toastStore.successResponseToast(response);
        emit("renamed", response.file);
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
        :header="header"
        :draggable="false"
        class="w-[min(420px,calc(100vw-2rem))] rounded-2xl border-edge bg-panel"
        @show="reset"
    >
        <div class="flex flex-col gap-1">
            <ValidationError :is-required="true" :message="r$.name.$errors[0]">
                <label for="rename-name">Name</label>
            </ValidationError>
            <InputText
                id="rename-name"
                ref="inputRef"
                v-model="form.name"
                class="w-full rounded-xl"
                autofocus
                @keydown.enter="rename"
            />
        </div>

        <template #footer>
            <Button label="Cancel" severity="secondary" variant="outlined" @click="visible = false" />
            <Button
                :label="loading ? 'Renaming...' : 'Rename'"
                :icon="loading ? 'pi pi-spin pi-spinner mr-2' : ''"
                :disabled="loading"
                @click="rename"
            />
        </template>
    </Dialog>
</template>
