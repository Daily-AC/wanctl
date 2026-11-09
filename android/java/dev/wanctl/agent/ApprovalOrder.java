package dev.wanctl.agent;

import java.util.Comparator;
import java.util.List;

/** The home-screen order, kept free of Android APIs so it can be tested alone. */
final class ApprovalOrder {
    interface Item {
        String state();

        long created();
    }

    private ApprovalOrder() {
    }

    static <T extends Item> void sort(List<T> cards) {
        cards.sort(Comparator
                .comparingInt((T k) -> priority(k.state()))
                .thenComparingLong(Item::created));
    }

    private static int priority(String state) {
        if ("pending".equals(state)) {
            return 0;
        }
        if ("expired".equals(state)) {
            return 1;
        }
        return 2;
    }
}
