import { computed } from "vue";
import { useRoute, useRouter } from "vue-router";
import type { ComputedRef } from "vue";
import type { LocationQuery, RouteLocationRaw } from "vue-router";

export interface ViewRoute {
    viewId: ComputedRef<number | null>;
    viewRoute: (id: number) => RouteLocationRaw;
    show: (id: number) => Promise<void>;
    close: () => Promise<void>;
}

// The file open in the viewer is the view param of the list page, such as /?dir=/Photos&view=12, so back and reload keep it.
// Open it with a link or a push to viewRoute. show moves to another file in the same history entry, so one back
// closes the viewer. close goes back when the viewer opened from this page, and else drops the param.
export function useViewRoute(): ViewRoute {
    const route = useRoute();
    const router = useRouter();

    const viewId = computed<number | null>(() => {
        const id = Number(route.query.view);
        return Number.isSafeInteger(id) && id > 0 ? id : null;
    });

    function viewRoute(id: number): RouteLocationRaw {
        return { query: { ...route.query, view: String(id) } };
    }

    async function show(id: number): Promise<void> {
        await router.replace(viewRoute(id));
    }

    async function close(): Promise<void> {
        const query: LocationQuery = { ...route.query };
        delete query.view;
        const closed = router.resolve({ query });
        if (router.options.history.state.back === closed.fullPath) {
            router.back();
            return;
        }
        await router.replace(closed);
    }

    return { viewId, viewRoute, show, close };
}
